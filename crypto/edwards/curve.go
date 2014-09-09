package edwards

import (
	"fmt"
	"errors"
	"math/big"
	"crypto/cipher"
	"dissent/crypto"
)

var zero = big.NewInt(0)
var one = big.NewInt(1)


// Extension of Point interface for elliptic curve X,Y coordinate access
type point interface {
	crypto.Point

	initXY(x,y *big.Int, curve crypto.Group)

	getXY() (x,y *crypto.ModInt)
}

// Interface representing curve-specific methods of encoding points
// into a uniform representation (e.g., Elligator 1, 2, or Squared).
type hiding interface {
	HideLen() int
	HideEncode(p point, rand cipher.Stream) []byte
	HideDecode(p point, representative []byte)
}

// Generic "abstract base class" for Edwards curves,
// embodying functionality independent of internal Point representation.
type curve struct {
	self crypto.Group	// "Self pointer" for derived class
	Param			// Twisted Edwards curve parameters
	zero,one crypto.ModInt	// Constant ModInts with correct modulus
	a,d crypto.ModInt	// Curve equation parameters as ModInts
	full bool		// True if we're using the full group

	order crypto.ModInt	// Order of appropriate subgroup as a ModInt
	cofact crypto.ModInt	// Group's cofactor as a ModInt

	null crypto.Point	// Identity point for this group

	hide hiding		// Uniform point encoding method
}

func (c *curve) PrimeOrder() bool {
	return !c.full
}

// Returns the size in bytes of an encoded Secret for this curve.
func (c *curve) SecretLen() int {
	return (c.order.V.BitLen() + 7) / 8
}

// Create a new Secret for this curve.
func (c *curve) Secret() crypto.Secret {
	return crypto.NewModInt(0, &c.order.V)
}

// Returns the size in bytes of an encoded Point on this curve.
// Uses compressed representation consisting of the y-coordinate
// and only the sign bit of the x-coordinate.
func (c *curve) PointLen() int {
	return (c.P.BitLen() + 7 + 1) / 8
}

// Initialize a twisted Edwards curve with given parameters.
// Caller passes pointers to null and base point prototypes to be initialized.
func (c *curve) init(self crypto.Group, p *Param, fullGroup bool,
			null,base point) *curve {
	c.self = self
	c.Param = *p
	c.full = fullGroup
	c.null = null

	// Edwards curve parameters as ModInts for convenience
	c.a.Init(&p.A,&p.P)
	c.d.Init(&p.D,&p.P)

	// Cofactor
	c.cofact.Init64(int64(p.R), &c.P)

	// Determine the modulus for secrets on this curve.
	// Note that we do NOT initialize c.order with Init(),
	// as that would normalize to the modulus, resulting in zero.
	// Just to be sure it's never used, we leave c.order.M set to nil.
	// We want it to be in a ModInt so we can pass it to P.Mul(),
	// but the secret's modulus isn't needed for point multiplication.
	if fullGroup {
		// Secret modulus is prime-order times the ccofactor
		c.order.V.SetInt64(int64(p.R)).Mul(&c.order.V, &p.Q)
	} else {
		c.order.V.Set(&p.Q)	// Prime-order subgroup
	}

	// Useful ModInt constants for this curve
	c.zero.Init64(0, &c.P)
	c.one.Init64(1, &c.P)

	// Identity element is (0,1)
	null.initXY(zero, one, self)

	// Base point B
	var bx,by *big.Int
	if !fullGroup {
		bx,by = &p.PBX, &p.PBY
	} else {
		bx,by = &p.FBX, &p.FBY
		base.initXY(&p.FBX, &p.FBY, self)
	}
	if by.Sign() == 0 {
		// No standard base point was defined, so pick one.
		// Find the lowest-numbered y-coordinate that works.
		//println("Picking base point:")
		var x,y crypto.ModInt
		for y.Init64(2,&c.P); ; y.Add(&y,&c.one) {
			if !c.solveForX(&x,&y) {
				continue	// try another y
			}
			if c.coordSign(&x) != 0 {
				x.Neg(&x)	// try positive x first
			}
			base.initXY(&x.V, &y.V, self)
			if c.validPoint(base) {
				break		// got one
			}
			x.Neg(&x)		// try -bx
			if c.validPoint(base) {
				break		// got one
			}
		}
		//println("BX: "+x.V.String())
		//println("BY: "+y.V.String())
		bx,by = &x.V,&y.V
	}
	base.initXY(bx, by, self)

	// Uniform representation encoding methods,
	// only useful when using the full group.
	// (Points taken from the subgroup would be trivially recognizable.)
	if fullGroup {
		if p.Elligator1s.Sign() != 0 {
			c.hide = new(el1param).init(c, &p.Elligator1s)
		}
		// XXX Elligator2, Squared
	}

	// Sanity checks
	if !c.validPoint(null) {
		panic("invalid identity point "+null.String())
	}
	if !c.validPoint(base) {
		panic("invalid base point "+base.String())
	}

	return c
}

// Test the sign of an x or y coordinate.
// We use the least-significant bit of the coordinate as the sign bit.
func (c *curve) coordSign(i *crypto.ModInt) uint {
	return i.V.Bit(0)
}

// Convert a point to string representation.
func (c *curve) pointString(x,y *crypto.ModInt) string {
	return fmt.Sprintf("(%s,%s)", x.String(), y.String())
}

// Encode an Edwards curve point.
// We use little-endian encoding for consistency with Ed25519.
func (c *curve) encodePoint(x,y *crypto.ModInt) []byte {

	// Encode the y-coordinate
	b := y.Encode()

	// Encode the sign of the x-coordinate.
	if y.M.BitLen() & 7 == 0 {
		// No unused bits at the top of y-coordinate encoding,
		// so we must prepend a whole byte.
		b = append(make([]byte,1), b...)
	}
	if c.coordSign(x) != 0 {
		b[0] |= 0x80
	}

	// Convert to little-endian
	reverse(b,b)
	return b
}

// Decode an Edwards curve point into the given x,y coordinates.
// Returns an error if the input does not denote a valid curve point.
// Note that this does NOT check if the point is in the prime-order subgroup:
// an adversary could create an encoding denoting a point
// on the twist of the curve, or in a larger subgroup.
// However, the "safecurves" criteria (http://safecurves.cr.yp.to)
// ensure that none of these other subgroups are small
// other than the tiny ones represented by the cofactor;
// hence Diffie-Hellman exchange can be done without subgroup checking
// without exposing more than the least-significant bits of the secret.
func (c *curve) decodePoint(bb []byte, x,y *crypto.ModInt) error {

	// Convert from little-endian
	b := make([]byte, len(bb))
	reverse(b,bb)

	// Extract the sign of the x-coordinate
	xsign := uint(b[0] >> 7)
	b[0] &^= 0x80

	// Extract the y-coordinate
	y.V.SetBytes(b)

	// Compute the corresponding x-coordinate
	if !c.solveForX(x,y) {
		return errors.New("invalid elliptic curve point")
	}
	if c.coordSign(x) != xsign {
		x.Neg(x)
	}

	return nil
}

// Byte-reverse src into dst,
// so that src[0] goes into dst[len-1] and vice versa.
// dst and src may be the same slice but otherwise must not overlap.
// XXX this probably belongs in a utils package somewhere.
func reverse(dst,src []byte) []byte {
	l := len(dst)
	if len(src) != l {
		panic("different-length slices passed to reverse")
	}
	if &dst[0] == &src[0] {		// in-place
		for i := 0; i < l/2; i++ {
			t := dst[i]
			dst[i] = dst[l-1-i]
			dst[l-1-i] = t
		}
	} else {
		for i := 0; i < l; i++ {
			dst[i] = src[l-1-i]
		}
	}
	return dst
}

// Given a y-coordinate, solve for the x-coordinate on the curve,
// using the characteristic equation rewritten as:
//
//	x^2 = (1 - y^2)/(a - d*y^2)
//
// Returns true on success,
// false if there is no x-coordinate corresponding to the chosen y-coordinate.
//
func (c *curve) solveForX(x,y *crypto.ModInt) bool {
	var yy,t1,t2 crypto.ModInt

	yy.Mul(y,y)				// yy = y^2
	t1.Sub(&c.one,&yy)			// t1 = 1 - y^-2
	t2.Mul(&c.d,&yy).Sub(&c.a,&t2)		// t2 = a - d*y^2
	t2.Div(&t1,&t2)				// t2 = x^2
	return x.Sqrt(&t2)			// may fail if not a square
}

// Test if a supposed point is on the curve,
// by checking the characteristic equation for Edwards curves:
//
//	a*x^2 + y^2 = 1 + d*x^2*y^2
//
func (c *curve) onCurve(x,y *crypto.ModInt) bool {
	var xx,yy,l,r crypto.ModInt

	xx.Mul(x,x)				// xx = x^2
	yy.Mul(y,y)				// yy = y^2

	l.Mul(&c.a,&xx).Add(&l,&yy)		// l = a*x^2 + y^2
	r.Mul(&c.d,&xx).Mul(&r,&yy).Add(&c.one,&r)
						// r = 1 + d*x^2*y^2
	return l.Equal(&r)
}

// Sanity-check a point to ensure that it is on the curve
// and within the appropriate subgroup.
func (c *curve) validPoint(P point) bool {

	// Check on-curve
	x,y := P.getXY()
	if !c.onCurve(x,y) {
		return false
	}

	// Check in-subgroup by multiplying by subgroup order
	Q := c.self.Point()
	Q.Mul(P, &c.order)
	if !Q.Equal(c.null) {
		return false
	}

	return true
}

// Return number of bytes that can be embedded into points on this curve.
func (c *curve) pickLen() int {
	// Reserve at least 8 most-significant bits for randomness,
	// and the least-significant 8 bits for embedded data length.
	// (Hopefully it's unlikely we'll need >=2048-bit curves soon.)
	return (c.P.BitLen() - 8 - 8) / 8
}

// Pick a [pseudo-]random curve point with optional embedded data,
// filling in the point's x,y coordinates
// and returning any remaining data not embedded.
func (c *curve) pickPoint(P point, data []byte, rand cipher.Stream) []byte {

	// How much data to embed?
	dl := c.pickLen()
	if dl > len(data) {
		dl = len(data)
	}

	// Retry until we find a valid point
	var x,y crypto.ModInt
	var Q crypto.Point
	for {
		// Get random bits the size of a compressed Point encoding,
		// in which the topmost bit is reserved for the x-coord sign.
		l := c.PointLen()
		b := make([]byte, l)
		rand.XORKeyStream(b,b)		// Interpret as little-endian
		if data != nil {
			b[0] = byte(dl)		// Encode length in low 8 bits
			copy(b[1:1+dl],data)	// Copy in data to embed
		}
		reverse(b,b)			// Convert to big-endian form

		xsign := b[0] >> 7		// save x-coordinate sign bit
		b[0] &^= 0xff << uint(c.P.BitLen() & 7)	// clear high bits

		y.M = &c.P			// set y-coordinate
		y.SetBytes(b)

		if !c.solveForX(&x,&y) {	// Corresponding x-coordinate?
			continue		// none, retry
		}

		// Pick a random sign for the x-coordinate
		if c.coordSign(&x) != uint(xsign) {
			x.Neg(&x)
		}

		// Initialize the point
		P.initXY(&x.V, &y.V, c.self)
		if c.full {
			// If we're using the full group,
			// we just need any point on the curve, so we're done.
			return data[dl:]
		}

		// We're using the prime-order subgroup,
		// so we need to make sure the point is in that subgroup.
		// If we're not trying to embed data,
		// we can convert our point into one in the subgroup
		// simply by multiplying it by the cofactor.
		if data == nil {
			P.Mul(P, &c.cofact)	// multiply by cofactor
			if P.Equal(c.null) {
				continue	// unlucky; try again
			}
			return data[dl:]
		}

		// Since we need the point's y-coordinate to make sense,
		// we must simply check if the point is in the subgroup
		// and retry point generation until it is.
		if Q == nil {
			Q = c.self.Point()
		}
		Q.Mul(P, &c.order)
		if Q.Equal(c.null) {
			return data[dl:]
		}

		// Keep trying...
	}
}

// Extract embedded data from a point group element,
// or an error if embedded data is invalid or not present.
func (c *curve) data(x,y *crypto.ModInt) ([]byte,error) {
	b := c.encodePoint(x,y)
	dl := int(b[0])
	if dl > c.pickLen() {
		return nil,errors.New("invalid embedded data length")
	}
	return b[1:1+dl],nil
}

