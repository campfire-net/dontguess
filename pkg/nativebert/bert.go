package nativebert

import (
	"fmt"
	"math"
)

// config holds the all-MiniLM-L6-v2 (BertModel) hyperparameters. These are
// fixed for this checkpoint and verified against config.json.
type config struct {
	hidden       int     // 384
	layers       int     // 6
	heads        int     // 12
	headDim      int     // 32 = hidden/heads
	intermediate int     // 1536
	eps          float32 // 1e-12 (layer_norm_eps)
}

func defaultConfig() config {
	return config{hidden: 384, layers: 6, heads: 12, headDim: 32, intermediate: 1536, eps: 1e-12}
}

// linear is a dense layer y = x·Wᵀ + b. Weights follow the PyTorch/safetensors
// convention: W has shape [out, in] stored row-major, so row o holds the
// weights that produce output o.
type linear struct {
	w    []float32 // len = out*in, row-major [out][in]
	b    []float32 // len = out
	out  int
	in   int
}

// layerNorm holds elementwise affine parameters for a LayerNorm over the last
// (hidden) dimension.
type layerNorm struct {
	gamma []float32
	beta  []float32
}

// encoderLayer is one BERT transformer block.
type encoderLayer struct {
	query, key, value linear
	attnOut           linear
	attnLN            layerNorm
	intermediate      linear
	output            linear
	outLN             layerNorm
}

// bertModel is the loaded, ready-to-run MiniLM encoder.
type bertModel struct {
	cfg config

	wordEmb []float32 // [vocab][hidden]
	posEmb  []float32 // [maxPos][hidden]
	typeEmb []float32 // [2][hidden]
	embLN   layerNorm

	layers []encoderLayer
}

// newBertModel assembles a bertModel from the named safetensors tensors.
func newBertModel(t map[string]tensor, cfg config) (*bertModel, error) {
	get := func(name string) (tensor, error) {
		ts, ok := t[name]
		if !ok {
			return tensor{}, fmt.Errorf("nativebert: missing weight %q", name)
		}
		return ts, nil
	}
	lin := func(prefix string) (linear, error) {
		w, err := get(prefix + ".weight")
		if err != nil {
			return linear{}, err
		}
		b, err := get(prefix + ".bias")
		if err != nil {
			return linear{}, err
		}
		if len(w.shape) != 2 {
			return linear{}, fmt.Errorf("nativebert: %q.weight is not 2-D: %v", prefix, w.shape)
		}
		return linear{w: w.data, b: b.data, out: w.shape[0], in: w.shape[1]}, nil
	}
	ln := func(prefix string) (layerNorm, error) {
		g, err := get(prefix + ".weight")
		if err != nil {
			return layerNorm{}, err
		}
		b, err := get(prefix + ".bias")
		if err != nil {
			return layerNorm{}, err
		}
		return layerNorm{gamma: g.data, beta: b.data}, nil
	}

	m := &bertModel{cfg: cfg}
	var err error
	we, err := get("embeddings.word_embeddings.weight")
	if err != nil {
		return nil, err
	}
	pe, err := get("embeddings.position_embeddings.weight")
	if err != nil {
		return nil, err
	}
	te, err := get("embeddings.token_type_embeddings.weight")
	if err != nil {
		return nil, err
	}
	m.wordEmb, m.posEmb, m.typeEmb = we.data, pe.data, te.data
	if m.embLN, err = ln("embeddings.LayerNorm"); err != nil {
		return nil, err
	}

	m.layers = make([]encoderLayer, cfg.layers)
	for i := 0; i < cfg.layers; i++ {
		p := fmt.Sprintf("encoder.layer.%d.", i)
		var l encoderLayer
		if l.query, err = lin(p + "attention.self.query"); err != nil {
			return nil, err
		}
		if l.key, err = lin(p + "attention.self.key"); err != nil {
			return nil, err
		}
		if l.value, err = lin(p + "attention.self.value"); err != nil {
			return nil, err
		}
		if l.attnOut, err = lin(p + "attention.output.dense"); err != nil {
			return nil, err
		}
		if l.attnLN, err = ln(p + "attention.output.LayerNorm"); err != nil {
			return nil, err
		}
		if l.intermediate, err = lin(p + "intermediate.dense"); err != nil {
			return nil, err
		}
		if l.output, err = lin(p + "output.dense"); err != nil {
			return nil, err
		}
		if l.outLN, err = ln(p + "output.LayerNorm"); err != nil {
			return nil, err
		}
		m.layers[i] = l
	}
	return m, nil
}

// embed runs the full forward pass for a single token-id sequence and returns
// the L2-normalized, mean-pooled 384-dim sentence embedding. Because encode
// emits no padding, every position is a real token: attention needs no mask
// and mean pooling is a plain average over all positions.
func (m *bertModel) embed(ids []int) []float32 {
	seq := len(ids)
	if seq == 0 {
		return make([]float32, m.cfg.hidden)
	}
	h := m.cfg.hidden

	// Embedding lookup: word + position + token_type(0), then LayerNorm.
	x := make([][]float32, seq)
	for s, id := range ids {
		row := make([]float32, h)
		wOff := id * h
		pOff := s * h // position id == index (contiguous, type_id 0)
		for k := 0; k < h; k++ {
			row[k] = m.wordEmb[wOff+k] + m.posEmb[pOff+k] + m.typeEmb[k]
		}
		x[s] = row
	}
	applyLayerNorm(x, m.embLN, m.cfg.eps)

	for li := range m.layers {
		x = m.encoderForward(x, &m.layers[li])
	}

	// Mean pooling over tokens.
	pooled := make([]float32, h)
	for _, row := range x {
		for k := 0; k < h; k++ {
			pooled[k] += row[k]
		}
	}
	inv := float32(1) / float32(seq)
	for k := 0; k < h; k++ {
		pooled[k] *= inv
	}

	// L2 normalize.
	var norm2 float64
	for _, v := range pooled {
		norm2 += float64(v) * float64(v)
	}
	nrm := float32(math.Sqrt(norm2))
	if nrm < 1e-9 {
		nrm = 1e-9
	}
	for k := 0; k < h; k++ {
		pooled[k] /= nrm
	}
	return pooled
}

// encoderForward runs one transformer block: multi-head self-attention with a
// residual + LayerNorm, then a GELU feed-forward with a residual + LayerNorm.
func (m *bertModel) encoderForward(x [][]float32, l *encoderLayer) [][]float32 {
	seq := len(x)
	h := m.cfg.hidden
	heads, hd := m.cfg.heads, m.cfg.headDim

	q := applyLinear(x, l.query)
	k := applyLinear(x, l.key)
	v := applyLinear(x, l.value)

	scale := float32(1) / float32(math.Sqrt(float64(hd)))
	ctx := make([][]float32, seq)
	for s := range ctx {
		ctx[s] = make([]float32, h)
	}

	scores := make([]float32, seq)
	for hi := 0; hi < heads; hi++ {
		base := hi * hd
		for i := 0; i < seq; i++ {
			// Attention scores of query i against every key j.
			for j := 0; j < seq; j++ {
				var dot float32
				qi, kj := q[i][base:base+hd], k[j][base:base+hd]
				for d := 0; d < hd; d++ {
					dot += qi[d] * kj[d]
				}
				scores[j] = dot * scale
			}
			softmax(scores[:seq])
			// Weighted sum of value vectors into this head's context slice.
			out := ctx[i][base : base+hd]
			for j := 0; j < seq; j++ {
				w := scores[j]
				vj := v[j][base : base+hd]
				for d := 0; d < hd; d++ {
					out[d] += w * vj[d]
				}
			}
		}
	}

	// Attention output projection + residual + LayerNorm.
	attn := applyLinear(ctx, l.attnOut)
	addInPlace(attn, x)
	applyLayerNorm(attn, l.attnLN, m.cfg.eps)

	// Feed-forward: GELU(intermediate) → output, + residual + LayerNorm.
	inter := applyLinear(attn, l.intermediate)
	for s := range inter {
		for k := range inter[s] {
			inter[s][k] = gelu(inter[s][k])
		}
	}
	out := applyLinear(inter, l.output)
	addInPlace(out, attn)
	applyLayerNorm(out, l.outLN, m.cfg.eps)
	return out
}

// applyLinear computes y = x·Wᵀ + b for every row of x.
func applyLinear(x [][]float32, l linear) [][]float32 {
	out := make([][]float32, len(x))
	for s, row := range x {
		y := make([]float32, l.out)
		for o := 0; o < l.out; o++ {
			wRow := l.w[o*l.in : o*l.in+l.in]
			var acc float32
			for i := 0; i < l.in; i++ {
				acc += row[i] * wRow[i]
			}
			y[o] = acc + l.b[o]
		}
		out[s] = y
	}
	return out
}

// applyLayerNorm normalizes each row over the hidden dimension in place using
// the biased (population) variance, matching BERT/PyTorch LayerNorm.
func applyLayerNorm(x [][]float32, ln layerNorm, eps float32) {
	for _, row := range x {
		n := len(row)
		var mean float64
		for _, v := range row {
			mean += float64(v)
		}
		mean /= float64(n)
		var variance float64
		for _, v := range row {
			d := float64(v) - mean
			variance += d * d
		}
		variance /= float64(n)
		inv := 1.0 / math.Sqrt(variance+float64(eps))
		for k, v := range row {
			row[k] = float32((float64(v)-mean)*inv)*ln.gamma[k] + ln.beta[k]
		}
	}
}

// addInPlace adds b into a elementwise (residual connection).
func addInPlace(a, b [][]float32) {
	for s := range a {
		for k := range a[s] {
			a[s][k] += b[s][k]
		}
	}
}

// softmax normalizes a score slice in place.
func softmax(s []float32) {
	maxv := s[0]
	for _, v := range s[1:] {
		if v > maxv {
			maxv = v
		}
	}
	var sum float32
	for i, v := range s {
		e := float32(math.Exp(float64(v - maxv)))
		s[i] = e
		sum += e
	}
	inv := 1 / sum
	for i := range s {
		s[i] *= inv
	}
}

// gelu is the exact Gaussian Error Linear Unit (erf form), matching
// hidden_act="gelu" in HF transformers (not the tanh approximation).
func gelu(x float32) float32 {
	return float32(0.5 * float64(x) * (1.0 + math.Erf(float64(x)/math.Sqrt2)))
}
