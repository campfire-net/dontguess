package nativebert

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Special-token ids for the all-MiniLM-L6-v2 (bert-base uncased) vocab.
// These are stable across the uncased BERT family and re-verified against the
// loaded vocab at construction time.
const (
	tokCLS = "[CLS]"
	tokSEP = "[SEP]"
	tokUNK = "[UNK]"
	tokPAD = "[PAD]"

	maxSeqLen        = 128 // matches the reference (enable_truncation(max_length=128))
	maxCharsPerWord  = 100 // WordPiece: words longer than this map straight to [UNK]
	subwordPrefixLen = 2   // len("##")
)

// wordPiece is a pure-Go BERT WordPiece tokenizer equivalent to the HuggingFace
// `tokenizer.json` shipped with all-MiniLM-L6-v2: BertNormalizer (clean text,
// split CJK, lowercase, strip accents) → BertPreTokenizer (whitespace + split
// on punctuation) → greedy longest-match WordPiece with the "##" continuation
// prefix → TemplateProcessing wrapping [CLS] … [SEP].
type wordPiece struct {
	vocab            map[string]int
	clsID, sepID     int
	unkID            int
	maxInputChars    int
}

// tokenizerJSON is the minimal subset of the HF tokenizer.json we consume:
// the WordPiece vocab lives under model.vocab.
type tokenizerJSON struct {
	Model struct {
		Vocab map[string]int `json:"vocab"`
	} `json:"model"`
}

// newWordPiece builds a tokenizer from raw tokenizer.json bytes.
func newWordPiece(tokenizerJSONBytes []byte) (*wordPiece, error) {
	var tj tokenizerJSON
	if err := json.Unmarshal(tokenizerJSONBytes, &tj); err != nil {
		return nil, fmt.Errorf("wordpiece: parse tokenizer.json: %w", err)
	}
	vocab := tj.Model.Vocab
	if len(vocab) == 0 {
		return nil, fmt.Errorf("wordpiece: tokenizer.json has empty model.vocab")
	}
	wp := &wordPiece{vocab: vocab, maxInputChars: maxCharsPerWord}
	for _, s := range []struct {
		name string
		dst  *int
	}{{tokCLS, &wp.clsID}, {tokSEP, &wp.sepID}, {tokUNK, &wp.unkID}} {
		id, ok := vocab[s.name]
		if !ok {
			return nil, fmt.Errorf("wordpiece: vocab missing special token %q", s.name)
		}
		*s.dst = id
	}
	return wp, nil
}

// encode tokenizes text into input ids, wrapped with [CLS] and [SEP] and
// truncated so the total length (including both special tokens) is at most
// maxSeqLen. No padding is applied — the forward pass masks nothing because
// every returned position is a real token.
func (wp *wordPiece) encode(text string) []int {
	pieces := wp.wordpieceAll(text)

	// Reserve two slots for [CLS] and [SEP].
	if len(pieces) > maxSeqLen-2 {
		pieces = pieces[:maxSeqLen-2]
	}

	ids := make([]int, 0, len(pieces)+2)
	ids = append(ids, wp.clsID)
	ids = append(ids, pieces...)
	ids = append(ids, wp.sepID)
	return ids
}

// wordpieceAll runs normalization, pre-tokenization, and WordPiece over the
// whole text, returning the flat sequence of subword ids (no special tokens).
func (wp *wordPiece) wordpieceAll(text string) []int {
	var ids []int
	for _, word := range basicTokenize(text) {
		ids = append(ids, wp.wordpieceWord(word)...)
	}
	return ids
}

// wordpieceWord applies greedy longest-match WordPiece to a single
// pre-tokenized word. Continuation subwords carry the "##" prefix.
func (wp *wordPiece) wordpieceWord(word string) []int {
	runes := []rune(word)
	if len(runes) > wp.maxInputChars {
		return []int{wp.unkID}
	}

	var out []int
	start := 0
	for start < len(runes) {
		end := len(runes)
		curID := -1
		for start < end {
			sub := string(runes[start:end])
			if start > 0 {
				sub = "##" + sub
			}
			if id, ok := wp.vocab[sub]; ok {
				curID = id
				break
			}
			end--
		}
		if curID == -1 {
			// No prefix of the remaining suffix is in the vocab: the entire
			// word is unknown (BERT emits a single [UNK] for the whole word).
			return []int{wp.unkID}
		}
		out = append(out, curID)
		start = end
	}
	return out
}

// basicTokenize reproduces BertNormalizer + BertPreTokenizer: clean control
// chars, pad CJK characters with spaces, lowercase, strip accents, then split
// on whitespace and isolate punctuation into standalone tokens.
func basicTokenize(text string) []string {
	// Pass 1 — clean_text + handle_chinese_chars: drop control/invalid runes,
	// collapse whitespace to a single space, and pad CJK ideographs so each
	// becomes its own token.
	var cleaned strings.Builder
	for _, r := range text {
		if r == 0 || r == 0xFFFD || isControl(r) {
			continue
		}
		if isWhitespace(r) {
			cleaned.WriteByte(' ')
			continue
		}
		if isCJK(r) {
			cleaned.WriteByte(' ')
			cleaned.WriteRune(r)
			cleaned.WriteByte(' ')
			continue
		}
		cleaned.WriteRune(r)
	}

	// Pass 2 — strip_accents + lowercase: NFD-decompose so precomposed
	// letters (é → e + U+0301) split into base + combining mark, then drop
	// the combining marks (category Mn) and lowercase. This matches HF's
	// BertNormalizer(strip_accents=None, lowercase=True).
	decomposed := norm.NFD.String(cleaned.String())
	var b strings.Builder
	for _, r := range decomposed {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}

	var tokens []string
	for _, word := range strings.Fields(b.String()) {
		tokens = append(tokens, splitPunctuation(word)...)
	}
	return tokens
}

// splitPunctuation isolates each punctuation rune in a word into its own
// token, matching BERT's _run_split_on_punc.
func splitPunctuation(word string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range word {
		if isPunctuation(r) {
			flush()
			tokens = append(tokens, string(r))
			continue
		}
		cur.WriteRune(r)
	}
	flush()
	return tokens
}

func isControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

func isWhitespace(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' || r == ' ' {
		return true
	}
	return unicode.IsSpace(r)
}

// isPunctuation matches BERT's _is_punctuation: all ASCII non-alphanumeric
// printable chars are punctuation, plus any Unicode punctuation category.
func isPunctuation(r rune) bool {
	if (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126) {
		return true
	}
	return unicode.IsPunct(r)
}

// isCJK matches BERT's _is_chinese_char CJK unified ideograph ranges.
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF,
		r >= 0x3400 && r <= 0x4DBF,
		r >= 0x20000 && r <= 0x2A6DF,
		r >= 0x2A700 && r <= 0x2B73F,
		r >= 0x2B740 && r <= 0x2B81F,
		r >= 0x2B820 && r <= 0x2CEAF,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0x2F800 && r <= 0x2FA1F:
		return true
	}
	return false
}
