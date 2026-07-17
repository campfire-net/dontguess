package nativebert

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
)

// tensor is a loaded float32 tensor with its shape. Weights are stored in
// row-major order, matching the safetensors on-disk layout.
type tensor struct {
	shape []int
	data  []float32
}

// rows returns the size of the leading dimension (shape[0]).
func (t tensor) rows() int {
	if len(t.shape) == 0 {
		return 0
	}
	return t.shape[0]
}

// cols returns the product of every dimension after the first, i.e. the
// row stride. For a 2-D [out, in] weight this is `in`.
func (t tensor) cols() int {
	if len(t.shape) < 2 {
		return 1
	}
	n := 1
	for _, d := range t.shape[1:] {
		n *= d
	}
	return n
}

// safetensorsHeader mirrors the per-tensor metadata in a safetensors file:
// an 8-byte little-endian header length, then a JSON object mapping each
// tensor name to {dtype, shape, data_offsets:[start,end)} where offsets are
// relative to the byte region that follows the header.
type safetensorsEntry struct {
	Dtype   string `json:"dtype"`
	Shape   []int  `json:"shape"`
	Offsets []int  `json:"data_offsets"`
}

// parseSafetensors decodes a safetensors byte buffer into named float32
// tensors. F32 tensors are read directly; the sole non-F32 tensor in the
// MiniLM checkpoint (embeddings.position_ids, I64) is not needed for the
// forward pass and is skipped rather than converted.
func parseSafetensors(buf []byte) (map[string]tensor, error) {
	if len(buf) < 8 {
		return nil, fmt.Errorf("safetensors: buffer too small (%d bytes)", len(buf))
	}
	headerLen := binary.LittleEndian.Uint64(buf[:8])
	if headerLen == 0 || 8+headerLen > uint64(len(buf)) {
		return nil, fmt.Errorf("safetensors: bad header length %d (buffer %d)", headerLen, len(buf))
	}
	headerJSON := buf[8 : 8+headerLen]
	body := buf[8+headerLen:]

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(headerJSON, &raw); err != nil {
		return nil, fmt.Errorf("safetensors: parse header: %w", err)
	}

	out := make(map[string]tensor, len(raw))
	for name, msg := range raw {
		if name == "__metadata__" {
			continue
		}
		var e safetensorsEntry
		if err := json.Unmarshal(msg, &e); err != nil {
			return nil, fmt.Errorf("safetensors: parse entry %q: %w", name, err)
		}
		if e.Dtype != "F32" {
			// position_ids (I64) is the only non-F32 tensor and is unused.
			continue
		}
		if len(e.Offsets) != 2 || e.Offsets[0] < 0 || e.Offsets[1] > len(body) || e.Offsets[0] > e.Offsets[1] {
			return nil, fmt.Errorf("safetensors: entry %q has invalid data_offsets %v (body %d)", name, e.Offsets, len(body))
		}
		region := body[e.Offsets[0]:e.Offsets[1]]

		n := 1
		for _, d := range e.Shape {
			n *= d
		}
		if n*4 != len(region) {
			return nil, fmt.Errorf("safetensors: entry %q shape %v implies %d floats but region is %d bytes", name, e.Shape, n, len(region))
		}

		data := make([]float32, n)
		for i := 0; i < n; i++ {
			bits := binary.LittleEndian.Uint32(region[i*4:])
			data[i] = math.Float32frombits(bits)
		}
		out[name] = tensor{shape: e.Shape, data: data}
	}
	return out, nil
}
