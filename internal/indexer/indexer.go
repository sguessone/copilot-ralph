package indexer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var tokenRE = regexp.MustCompile(`\p{L}[\p{L}\p{N}_]*|[0-9]+`)

type Chunk struct {
	ID     string             `json:"id"`
	Path   string             `json:"path"`
	Text   string             `json:"text"`
	Vector map[string]float64 `json:"vector"`
}

type Index struct {
	Chunks []Chunk `json:"chunks"`
}

// IndexRepo walks rootDir and indexes textual files returning an in-memory index.
func IndexRepo(rootDir string) (*Index, error) {
	idx := &Index{Chunks: []Chunk{}}

	// file extensions to index
	exts := map[string]bool{
		".go": true, ".md": true, ".txt": true, ".yaml": true, ".yml": true,
		".json": true, ".js": true, ".ts": true, ".html": true,
	}

	idCounter := 0

	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !exts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		content, err := io.ReadAll(f)
		if err != nil {
			return nil
		}

		// Chunk by lines approx every 800-1200 chars
		text := string(content)
		for _, part := range chunkText(text, 1000) {
			idCounter++
			vec := buildVector(part)
			idx.Chunks = append(idx.Chunks, Chunk{
				ID:     fmt.Sprintf("c-%d", idCounter),
				Path:   path,
				Text:   part,
				Vector: vec,
			})
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return idx, nil
}

// chunkText splits text into chunks of approximately maxLen characters.
func chunkText(s string, maxLen int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var chunks []string
	reader := bufio.NewReader(strings.NewReader(s))
	var buf strings.Builder
	for {
		r, _, err := reader.ReadRune()
		if err == io.EOF {
			if buf.Len() > 0 {
				chunks = append(chunks, buf.String())
			}
			break
		}
		buf.WriteRune(r)
		if buf.Len() >= maxLen {
			// try to split at newline or space
			str := buf.String()
			// find last newline or period before end
			sep := strings.LastIndexAny(str, "\n.\n\r\n ")
			if sep <= 0 || sep < maxLen/2 {
				chunks = append(chunks, str)
				buf.Reset()
				continue
			}
			chunks = append(chunks, strings.TrimSpace(str[:sep+1]))
			rest := strings.TrimSpace(str[sep+1:])
			buf.Reset()
			buf.WriteString(rest)
		}
	}
	return chunks
}

// buildVector computes normalized term-frequency vector for text chunk.
func buildVector(s string) map[string]float64 {
	tokens := tokenRE.FindAllString(strings.ToLower(s), -1)
	if len(tokens) == 0 {
		return map[string]float64{}
	}
	counts := map[string]float64{}
	for _, t := range tokens {
		counts[t]++
	}
	// compute l2 norm
	var sum float64
	for _, v := range counts {
		sum += v * v
	}
	norm := 1.0
	if sum > 0 {
		norm = 1.0 / sqrt(sum)
	}
	vec := map[string]float64{}
	for k, v := range counts {
		vec[k] = v * norm
	}
	return vec
}

// sqrt uses simple Newton method to avoid importing math for brevity.
func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 16; i++ {
		z = 0.5 * (z + x/z)
	}
	return z
}

// Search returns top-k chunks matching query with cosine similarity.
type SearchResult struct {
	ChunkID string  `json:"chunk_id"`
	Path    string  `json:"path"`
	Text    string  `json:"text"`
	Score   float64 `json:"score"`
}

func (idx *Index) Search(query string, k int) []SearchResult {
	qvec := buildVector(query)
	if len(qvec) == 0 {
		return nil
	}
	var results []SearchResult
	for _, c := range idx.Chunks {
		score := dotProduct(qvec, c.Vector)
		if score <= 0 {
			continue
		}
		results = append(results, SearchResult{ChunkID: c.ID, Path: c.Path, Text: c.Text, Score: score})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if k > 0 && len(results) > k {
		results = results[:k]
	}
	return results
}

func dotProduct(a, b map[string]float64) float64 {
	var s float64
	for k, v := range a {
		if w, ok := b[k]; ok {
			s += v * w
		}
	}
	return s
}

// Save writes index as JSON to path.
func (idx *Index) Save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(idx)
}

// Load reads index from JSON file.
func Load(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var idx Index
	dec := json.NewDecoder(f)
	if err := dec.Decode(&idx); err != nil {
		return nil, err
	}
	return &idx, nil
}
