package main

// EXT_meshopt_compression decoding, ported from meshoptimizer's reference
// decoder (js/meshopt_decoder_reference.js, MIT, (C) 2016-2026 Arseny
// Kapoulkine; reference decoder by Jasper St. Pierre). Tripo's GLBs are
// meshopt-compressed, and three.js decodes that only with WebAssembly, which
// a widget sandbox may forbid, so the preview server and Blender get plain
// GLBs from here.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
)

func dezig(v uint32) uint32 { return (v >> 1) ^ -(v & 1) }

func decodeVertexBuffer(dst []byte, count, stride int, src []byte, filter string) error {
	if len(src) < 1 || (src[0] != 0xa0 && src[0] != 0xa1) {
		return errors.New("meshopt: unsupported vertex codec")
	}
	if stride <= 0 || stride > 256 || stride%4 != 0 || len(dst) < count*stride {
		return errors.New("meshopt: bad vertex stride")
	}
	version := src[0] & 0x0f
	maxBlock := min((0x2000/stride)&^0x0f, 0x100)
	deltas := make([]byte, maxBlock*stride)
	tailSize := stride
	if version != 0 {
		tailSize += stride / 4
	}
	tailPadded := max(tailSize, 32)
	if version != 0 {
		tailPadded = max(tailSize, 24)
	}
	if len(src) < 1+tailPadded {
		return errors.New("meshopt: vertex data too short")
	}
	tail := len(src) - tailSize
	temp := append([]byte(nil), src[tail:tail+stride]...)
	var channels []byte
	if version != 0 {
		channels = src[tail+stride : tail+tailSize]
	}
	limit := len(src) - tailPadded
	pos := 1
	need := func(n int) error {
		if pos+n > limit {
			return errors.New("meshopt: vertex data truncated")
		}
		return nil
	}
	headerModes := [3][4]int{{0, 2, 4, 8}, {0, 1, 2, 4}, {1, 2, 4, 8}}
	for base := 0; base < count; base += maxBlock {
		n := min(count-base, maxBlock)
		groups := (n + 15) >> 4
		headerBytes := (groups + 3) >> 2
		controlPos := pos
		if version != 0 {
			if err := need(stride / 4); err != nil {
				return err
			}
			pos += stride / 4
		}
		clear(deltas)
		for b := 0; b < stride; b++ {
			deltaBase := b * n
			control := 0
			if version != 0 {
				control = int(src[controlPos+(b>>2)]>>((b&3)<<1)) & 3
			}
			if control == 2 {
				continue // all zero
			}
			if control == 3 {
				if err := need(n); err != nil {
					return err
				}
				copy(deltas[deltaBase:], src[pos:pos+n])
				pos += n
				continue
			}
			if err := need(headerBytes); err != nil {
				return err
			}
			headerPos := pos
			pos += headerBytes
			row := 0
			if version != 0 {
				row = control + 1
			}
			for g := 0; g < groups; g++ {
				mode := int(src[headerPos+(g>>2)]>>((g&3)<<1)) & 3
				out := deltas[deltaBase+(g<<4):]
				switch headerModes[row][mode] {
				case 0:
				case 1:
					if err := need(2); err != nil {
						return err
					}
					hb := pos
					pos += 2
					for m := 0; m < 16; m++ {
						d := (src[hb+(m>>3)] >> (m & 7)) & 1
						if d == 1 {
							if err := need(1); err != nil {
								return err
							}
							d = src[pos]
							pos++
						}
						out[m] = d
					}
				case 2:
					if err := need(4); err != nil {
						return err
					}
					hb := pos
					pos += 4
					for m := 0; m < 16; m++ {
						d := (src[hb+(m>>2)] >> (6 - ((m & 3) << 1))) & 3
						if d == 3 {
							if err := need(1); err != nil {
								return err
							}
							d = src[pos]
							pos++
						}
						out[m] = d
					}
				case 4:
					if err := need(8); err != nil {
						return err
					}
					hb := pos
					pos += 8
					for m := 0; m < 16; m++ {
						d := (src[hb+(m>>1)] >> (4 - ((m & 1) << 2))) & 0x0f
						if d == 0x0f {
							if err := need(1); err != nil {
								return err
							}
							d = src[pos]
							pos++
						}
						out[m] = d
					}
				default:
					if err := need(16); err != nil {
						return err
					}
					copy(out[:16], src[pos:pos+16])
					pos += 16
				}
			}
		}
		for e := 0; e < n; e++ {
			o := (base + e) * stride
			for bg := 0; bg < stride; bg += 4 {
				ch := 0
				if version != 0 {
					ch = int(channels[bg>>2] & 3)
				}
				d := func(b int) uint32 { return uint32(deltas[b*n+e]) }
				switch ch {
				case 0:
					for b := bg; b < bg+4; b++ {
						temp[b] = byte(uint32(temp[b]) + dezig(d(b)))
						dst[o+b] = temp[b]
					}
				case 1:
					for b := bg; b < bg+4; b += 2 {
						v := uint32(binary.LittleEndian.Uint16(temp[b:])) + dezig(d(b)|d(b+1)<<8)
						binary.LittleEndian.PutUint16(temp[b:], uint16(v))
						copy(dst[o+b:o+b+2], temp[b:b+2])
					}
				case 2:
					delta := d(bg) | d(bg+1)<<8 | d(bg+2)<<16 | d(bg+3)<<24
					rot := int(channels[bg>>2] >> 4)
					v := binary.LittleEndian.Uint32(temp[bg:]) ^ bits.RotateLeft32(delta, -rot)
					binary.LittleEndian.PutUint32(temp[bg:], v)
					copy(dst[o+bg:o+bg+4], temp[bg:bg+4])
				default:
					return errors.New("meshopt: bad channel mode")
				}
			}
		}
	}
	if pos != limit {
		return errors.New("meshopt: vertex data has trailing bytes")
	}
	return applyFilter(dst[:count*stride], count, stride, filter)
}

func jsRound(x float64) float64 { return math.Floor(x + 0.5) } // JS Math.round

func applyFilter(dst []byte, count, stride int, filter string) error {
	le := binary.LittleEndian
	switch filter {
	case "", "NONE":
	case "OCTAHEDRAL":
		if stride != 4 && stride != 8 {
			return errors.New("meshopt: bad octahedral stride")
		}
		size := stride / 4
		maxInt := 127.0
		if size == 2 {
			maxInt = 32767
		}
		get := func(i int) float64 {
			if size == 1 {
				return float64(int8(dst[i]))
			}
			return float64(int16(le.Uint16(dst[i*2:])))
		}
		set := func(i int, v float64) {
			if size == 1 {
				dst[i] = byte(int8(v))
			} else {
				le.PutUint16(dst[i*2:], uint16(int16(v)))
			}
		}
		for i := 0; i < count*4; i += 4 {
			one := get(i + 2)
			x, y := get(i)/one, get(i+1)/one
			z := 1 - math.Abs(x) - math.Abs(y)
			t := math.Max(-z, 0)
			if x >= 0 {
				x -= t
			} else {
				x += t
			}
			if y >= 0 {
				y -= t
			} else {
				y += t
			}
			h := maxInt / math.Sqrt(x*x+y*y+z*z)
			set(i, jsRound(x*h))
			set(i+1, jsRound(y*h))
			set(i+2, jsRound(z*h))
		}
	case "QUATERNION":
		if stride != 8 {
			return errors.New("meshopt: bad quaternion stride")
		}
		for i := 0; i < count; i++ {
			q := dst[i*8:]
			c := func(k int) float64 { return float64(int16(le.Uint16(q[k*2:]))) }
			w16 := int16(le.Uint16(q[6:]))
			maxc := int(w16 & 3)
			s := math.Sqrt2 / 2 / float64(w16|3)
			x, y, z := c(0)*s, c(1)*s, c(2)*s
			w := math.Sqrt(math.Max(0, 1-x*x-y*y-z*z))
			put := func(k int, v float64) { le.PutUint16(q[((maxc+k)%4)*2:], uint16(int16(jsRound(v*32767)))) }
			put(1, x)
			put(2, y)
			put(3, z)
			put(0, w)
		}
	case "EXPONENTIAL":
		if stride%4 != 0 {
			return errors.New("meshopt: bad exponential stride")
		}
		for i := 0; i < count*stride; i += 4 {
			v := int32(le.Uint32(dst[i:]))
			exp, mantissa := v>>24, (v<<8)>>8
			f := math.Float32frombits(uint32(exp+127)<<23) * float32(mantissa)
			le.PutUint32(dst[i:], math.Float32bits(f))
		}
	case "COLOR":
		if stride != 4 && stride != 8 {
			return errors.New("meshopt: bad color stride")
		}
		size := stride / 4
		maxInt := float64(int(1)<<(stride*2) - 1)
		u := func(i int) int {
			if size == 1 {
				return int(dst[i])
			}
			return int(le.Uint16(dst[i*2:]))
		}
		sgn := func(i int) int {
			if size == 1 {
				return int(int8(dst[i]))
			}
			return int(int16(le.Uint16(dst[i*2:])))
		}
		set := func(i int, v float64) {
			if size == 1 {
				dst[i] = byte(int(v))
			} else {
				le.PutUint16(dst[i*2:], uint16(int(v)))
			}
		}
		for i := 0; i < count*4; i += 4 {
			y, co, cg, alpha := u(i), sgn(i+1), sgn(i+2), u(i+3)
			alphaBit := 31 - bits.LeadingZeros32(uint32(alpha))
			as := (1 << (alphaBit + 1)) - 1
			a := alpha & (as >> 1)
			a = (a << 1) | (a & 1)
			ss := maxInt / float64(as)
			set(i, jsRound(float64(y+co-cg)*ss))
			set(i+1, jsRound(float64(y+cg)*ss))
			set(i+2, jsRound(float64(y-co-cg)*ss))
			set(i+3, jsRound(float64(a)*ss))
		}
	default:
		return fmt.Errorf("meshopt: unknown filter %s", filter)
	}
	return nil
}

type fifo struct {
	v   []uint32
	off int
}

func (f *fifo) read(n int) uint32 { return f.v[(f.off-1-n)&(len(f.v)-1)] }
func (f *fifo) push(x uint32)     { f.v[f.off] = x; f.off = (f.off + 1) & (len(f.v) - 1) }

// leb reads an unsigned LEB128 value at *pos.
func leb(src []byte, pos *int) (uint32, error) {
	var n uint32
	for shift := 0; shift < 35; shift += 7 {
		if *pos >= len(src) {
			return 0, errors.New("meshopt: index data truncated")
		}
		b := src[*pos]
		*pos++
		n |= uint32(b&0x7f) << shift
		if b < 0x80 {
			return n, nil
		}
	}
	return 0, errors.New("meshopt: bad LEB128")
}

func putIndex(dst []byte, i int, v uint32, stride int) {
	if stride == 2 {
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(v))
	} else {
		binary.LittleEndian.PutUint32(dst[i*4:], v)
	}
}

func decodeIndexBuffer(dst []byte, count, stride int, src []byte) error {
	if len(src) < 17 || src[0] != 0xe1 {
		return errors.New("meshopt: unsupported index codec")
	}
	if count%3 != 0 || (stride != 2 && stride != 4) || len(dst) < count*stride {
		return errors.New("meshopt: bad index buffer")
	}
	tris := count / 3
	code, data, aux := 1, 1+tris, len(src)-16
	if data > aux {
		return errors.New("meshopt: index data truncated")
	}
	var next, last uint32
	edge, vert := &fifo{v: make([]uint32, 32)}, &fifo{v: make([]uint32, 16)}
	decode := func() (uint32, error) {
		v, err := leb(src, &data)
		last += dezig(v)
		return last, err
	}
	o := 0
	for i := 0; i < tris; i++ {
		c0 := src[code]
		code++
		b0, b1 := int(c0>>4), int(c0&0x0f)
		var a, b, c uint32
		var err error
		if b0 < 0x0f {
			a, b = edge.read(b0<<1), edge.read(b0<<1+1)
			switch {
			case b1 == 0:
				c = next
				next++
				vert.push(c)
			case b1 < 0x0d:
				c = vert.read(b1)
			case b1 == 0x0d:
				last--
				c = last
				vert.push(c)
			case b1 == 0x0e:
				last++
				c = last
				vert.push(c)
			default:
				if c, err = decode(); err != nil {
					return err
				}
				vert.push(c)
			}
			edge.push(b)
			edge.push(c)
			edge.push(c)
			edge.push(a)
		} else {
			var z, w int
			if b1 < 0x0e {
				e := src[aux+b1]
				z, w = int(e>>4), int(e&0x0f)
				a = next
				next++
				if z == 0 {
					b = next
					next++
				} else {
					b = vert.read(z - 1)
				}
				if w == 0 {
					c = next
					next++
				} else {
					c = vert.read(w - 1)
				}
				vert.push(a)
				if z == 0 {
					vert.push(b)
				}
				if w == 0 {
					vert.push(c)
				}
			} else {
				if data >= len(src) {
					return errors.New("meshopt: index data truncated")
				}
				e := src[data]
				data++
				if e == 0 {
					next = 0
				}
				z, w = int(e>>4), int(e&0x0f)
				if b1 == 0x0e {
					a = next
					next++
				} else if a, err = decode(); err != nil {
					return err
				}
				switch {
				case z == 0:
					b = next
					next++
				case z == 0x0f:
					if b, err = decode(); err != nil {
						return err
					}
				default:
					b = vert.read(z - 1)
				}
				switch {
				case w == 0:
					c = next
					next++
				case w == 0x0f:
					if c, err = decode(); err != nil {
						return err
					}
				default:
					c = vert.read(w - 1)
				}
				vert.push(a)
				if z == 0 || z == 0x0f {
					vert.push(b)
				}
				if w == 0 || w == 0x0f {
					vert.push(c)
				}
			}
			edge.push(a)
			edge.push(b)
			edge.push(b)
			edge.push(c)
			edge.push(c)
			edge.push(a)
		}
		putIndex(dst, o, a, stride)
		putIndex(dst, o+1, b, stride)
		putIndex(dst, o+2, c, stride)
		o += 3
	}
	return nil
}

func decodeIndexSequence(dst []byte, count, stride int, src []byte) error {
	if len(src) < 1 || src[0] != 0xd1 {
		return errors.New("meshopt: unsupported index sequence codec")
	}
	if (stride != 2 && stride != 4) || len(dst) < count*stride {
		return errors.New("meshopt: bad index sequence")
	}
	var last [2]uint32
	pos := 1
	for i := 0; i < count; i++ {
		v, err := leb(src, &pos)
		if err != nil {
			return err
		}
		b := v & 1
		last[b] += dezig(v >> 1)
		putIndex(dst, i, last[b], stride)
	}
	return nil
}

// decodeMeshoptGLB returns glb with every EXT_meshopt_compression bufferView
// decoded into one plain BIN chunk. Files without the extension come back as is.
func decodeMeshoptGLB(glb []byte) ([]byte, error) {
	le := binary.LittleEndian
	if len(glb) < 20 || le.Uint32(glb) != 0x46546C67 || le.Uint32(glb[4:]) != 2 {
		return nil, errors.New("not a GLB v2 file")
	}
	jsonLen := int(le.Uint32(glb[12:]))
	if le.Uint32(glb[16:]) != 0x4E4F534A || 20+jsonLen > len(glb) {
		return nil, errors.New("the GLB has no JSON chunk")
	}
	if !bytes.Contains(glb[20:20+jsonLen], []byte("EXT_meshopt_compression")) {
		return glb, nil
	}
	var bin []byte
	if rest := glb[20+jsonLen:]; len(rest) >= 8 && le.Uint32(rest[4:]) == 0x004E4942 {
		n := int(le.Uint32(rest))
		if 8+n > len(rest) {
			return nil, errors.New("the GLB BIN chunk is truncated")
		}
		bin = rest[8 : 8+n]
	}
	var doc map[string]any
	if err := json.Unmarshal(glb[20:20+jsonLen], &doc); err != nil {
		return nil, fmt.Errorf("the GLB JSON is invalid: %w", err)
	}
	buffers, _ := doc["buffers"].([]any)
	views, _ := doc["bufferViews"].([]any)
	// Only the GLB's own BIN (buffer 0 without a uri) holds data; the others
	// are meshopt fallback buffers.
	if len(buffers) == 0 || asString(buffers[0].(map[string]any)["uri"]) != "" {
		return nil, errors.New("the GLB's first buffer is not embedded")
	}
	num := func(m map[string]any, k string) int { f, _ := m[k].(float64); return int(f) }
	slice := func(buf, off, n int) ([]byte, error) {
		if buf != 0 || off < 0 || n < 0 || off+n > len(bin) {
			return nil, errors.New("a bufferView points outside the GLB's BIN chunk")
		}
		return bin[off : off+n], nil
	}
	var out bytes.Buffer
	for i, v := range views {
		bv, _ := v.(map[string]any)
		var data []byte
		var err error
		ext, _ := bv["extensions"].(map[string]any)
		if mo, ok := ext["EXT_meshopt_compression"].(map[string]any); ok {
			count, stride := num(mo, "count"), num(mo, "byteStride")
			var src []byte
			if src, err = slice(num(mo, "buffer"), num(mo, "byteOffset"), num(mo, "byteLength")); err != nil {
				return nil, err
			}
			data = make([]byte, count*stride)
			switch asString(mo["mode"]) {
			case "ATTRIBUTES":
				err = decodeVertexBuffer(data, count, stride, src, asString(mo["filter"]))
			case "TRIANGLES":
				err = decodeIndexBuffer(data, count, stride, src)
			case "INDICES":
				err = decodeIndexSequence(data, count, stride, src)
			default:
				err = fmt.Errorf("meshopt: unknown mode %v", mo["mode"])
			}
			if err != nil {
				return nil, fmt.Errorf("bufferView %d: %w", i, err)
			}
			delete(ext, "EXT_meshopt_compression")
			if len(ext) == 0 {
				delete(bv, "extensions")
			}
		} else if data, err = slice(num(bv, "buffer"), num(bv, "byteOffset"), num(bv, "byteLength")); err != nil {
			return nil, fmt.Errorf("bufferView %d: %w", i, err)
		}
		for out.Len()%4 != 0 {
			out.WriteByte(0)
		}
		bv["buffer"], bv["byteOffset"], bv["byteLength"] = 0, out.Len(), len(data)
		out.Write(data)
	}
	for out.Len()%4 != 0 {
		out.WriteByte(0)
	}
	doc["buffers"] = []any{map[string]any{"byteLength": out.Len()}}
	for _, key := range []string{"extensionsUsed", "extensionsRequired"} {
		list, _ := doc[key].([]any)
		kept := []any{}
		for _, e := range list {
			if e != "EXT_meshopt_compression" {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(doc, key)
		} else {
			doc[key] = kept
		}
	}
	js, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	for len(js)%4 != 0 {
		js = append(js, ' ')
	}
	var g bytes.Buffer
	total := 12 + 8 + len(js) + 8 + out.Len()
	for _, v := range []uint32{0x46546C67, 2, uint32(total), uint32(len(js)), 0x4E4F534A} {
		binary.Write(&g, le, v)
	}
	g.Write(js)
	binary.Write(&g, le, uint32(out.Len()))
	binary.Write(&g, le, uint32(0x004E4942))
	g.Write(out.Bytes())
	return g.Bytes(), nil
}
