package api

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// ContentBlock is a typed content block sent as a content_block SSE event.
// The frontend renders each block type with a specialized component.
type ContentBlock struct {
	Type string `json:"type"`

	// TextBlock
	Text string `json:"text,omitempty"`

	// CodeBlock
	Language string `json:"language,omitempty"`
	Code     string `json:"code,omitempty"`
	Filename string `json:"filename,omitempty"`

	// ChartBlock
	ChartType string     `json:"chart_type,omitempty"`
	Title     string     `json:"title,omitempty"`
	Data      *ChartData `json:"data,omitempty"`

	// DiagramBlock
	Format  string `json:"format,omitempty"`
	Source  string `json:"source,omitempty"`
	Caption string `json:"caption,omitempty"`

	// TableBlock
	Headers []string   `json:"headers,omitempty"`
	Rows    [][]string `json:"rows,omitempty"`

	// ComponentBlock — UI component with type + arbitrary props
	ComponentType string         `json:"component_type,omitempty"`
	Props         map[string]any `json:"props,omitempty"`
}

// ChartData holds chart datasets.
type ChartData struct {
	Labels   []string       `json:"labels"`
	Datasets []ChartDataset `json:"datasets"`
}

// ChartDataset is a single series in a chart.
type ChartDataset struct {
	Label  string    `json:"label"`
	Values []float64 `json:"values"`
	Color  string    `json:"color,omitempty"`
}

var fenceRe = regexp.MustCompile("(?s)```(\\w*)\\n(.*?)```")
var inlineSVGRe = regexp.MustCompile("(?s)(<svg[\\s\\S]*?</svg>)")

type blockMatch struct {
	start, end int
	lang, body string
	svg        string
}

// ParseBlocks parses LLM text output into structured content blocks.
func ParseBlocks(text string) []ContentBlock {
	if text == "" {
		return nil
	}

	var matches []blockMatch

	for _, m := range fenceRe.FindAllStringSubmatchIndex(text, -1) {
		lang := text[m[2]:m[3]]
		body := text[m[4]:m[5]]
		matches = append(matches, blockMatch{start: m[0], end: m[1], lang: lang, body: body})
	}

	for _, m := range inlineSVGRe.FindAllStringSubmatchIndex(text, -1) {
		svg := text[m[2]:m[3]]
		overlaps := false
		for _, fm := range matches {
			if m[0] >= fm.start && m[1] <= fm.end {
				overlaps = true
				break
			}
		}
		if !overlaps {
			matches = append(matches, blockMatch{start: m[0], end: m[1], svg: svg})
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].start < matches[j].start
	})

	var blocks []ContentBlock
	lastIndex := 0

	for _, m := range matches {
		before := strings.TrimSpace(text[lastIndex:m.start])
		if before != "" {
			blocks = appendTextOrTable(blocks, before)
		}

		if m.svg != "" {
			blocks = append(blocks, ContentBlock{
				Type:   "diagram",
				Format: "svg",
				Source: strings.TrimSpace(m.svg),
			})
		} else {
			lang := strings.ToLower(m.lang)
			body := strings.TrimSpace(m.body)

			switch lang {
			case "chart":
				blocks = appendChartBlock(blocks, body)
			case "mermaid":
				blocks = append(blocks, ContentBlock{
					Type:   "diagram",
					Format: "mermaid",
					Source: body,
				})
			case "svg":
				blocks = append(blocks, ContentBlock{
					Type:   "diagram",
					Format: "svg",
					Source: body,
				})
			case "component":
				if cb := buildComponentBlock(body); cb != nil {
					blocks = append(blocks, *cb)
				}
			default:
				if lang == "" {
					lang = "text"
				}
				blocks = append(blocks, ContentBlock{
					Type:     "code",
					Language: lang,
					Code:     body,
				})
			}
		}

		lastIndex = m.end
	}

	remaining := strings.TrimSpace(text[lastIndex:])
	if remaining != "" {
		blocks = appendTextOrTable(blocks, remaining)
	}

	// If only one text block, return nil — caller uses plain text rendering.
	if len(blocks) == 1 && blocks[0].Type == "text" {
		return nil
	}

	return blocks
}

func appendChartBlock(blocks []ContentBlock, body string) []ContentBlock {
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return append(blocks, ContentBlock{Type: "code", Language: "json", Code: body})
	}

	chartType := strVal(raw, "chart_type")
	if chartType == "" {
		chartType = strVal(raw, "type")
	}
	if chartType == "" {
		chartType = "bar"
	}

	title := strVal(raw, "title")
	if title == "" {
		if opts, ok := raw["options"].(map[string]any); ok {
			if plugins, ok := opts["plugins"].(map[string]any); ok {
				if titleObj, ok := plugins["title"].(map[string]any); ok {
					title = strVal(titleObj, "text")
				}
			}
		}
	}

	dataObj, _ := raw["data"].(map[string]any)
	if dataObj == nil {
		dataObj = map[string]any{}
	}

	labelsRaw, _ := dataObj["labels"].([]any)
	labels := make([]string, 0, len(labelsRaw))
	for _, l := range labelsRaw {
		if s, ok := l.(string); ok {
			labels = append(labels, s)
		}
	}

	datasetsRaw, _ := dataObj["datasets"].([]any)
	datasets := make([]ChartDataset, 0, len(datasetsRaw))
	for _, dsRaw := range datasetsRaw {
		ds, ok := dsRaw.(map[string]any)
		if !ok {
			continue
		}
		valuesRaw, _ := ds["values"].([]any)
		if valuesRaw == nil {
			valuesRaw, _ = ds["data"].([]any)
		}
		var values []float64
		for _, v := range valuesRaw {
			if f, ok := v.(float64); ok {
				values = append(values, f)
			}
		}
		datasets = append(datasets, ChartDataset{
			Label:  strVal(ds, "label"),
			Values: values,
			Color:  strVal(ds, "color"),
		})
	}

	return append(blocks, ContentBlock{
		Type:      "chart",
		ChartType: chartType,
		Title:     title,
		Data:      &ChartData{Labels: labels, Datasets: datasets},
	})
}

func appendTextOrTable(blocks []ContentBlock, text string) []ContentBlock {
	lines := strings.Split(text, "\n")
	i := 0
	textStart := 0

	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		if strings.Contains(line, "|") && i+1 < len(lines) {
			sep := strings.TrimSpace(lines[i+1])
			if isTableSep(sep) {
				if i > textStart {
					before := strings.TrimSpace(strings.Join(lines[textStart:i], "\n"))
					if before != "" {
						blocks = append(blocks, ContentBlock{Type: "text", Text: before})
					}
				}

				end := i + 2
				for end < len(lines) && strings.Contains(strings.TrimSpace(lines[end]), "|") {
					end++
				}

				headers := parsePipeLine(lines[i])
				rows := make([][]string, 0, end-i-2)
				for r := i + 2; r < end; r++ {
					rows = append(rows, parsePipeLine(lines[r]))
				}

				blocks = append(blocks, ContentBlock{
					Type:    "table",
					Headers: headers,
					Rows:    rows,
				})

				i = end
				textStart = end
				continue
			}
		}
		i++
	}

	if textStart < len(lines) {
		remaining := strings.TrimSpace(strings.Join(lines[textStart:], "\n"))
		if remaining != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: remaining})
		}
	}

	return blocks
}

var tableSepRe = regexp.MustCompile(`^\|?[\s\-:|]+\|[\s\-:|]+\|?$`)

func isTableSep(line string) bool {
	return tableSepRe.MatchString(line)
}

func parsePipeLine(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	result := make([]string, len(parts))
	for i, p := range parts {
		result[i] = strings.TrimSpace(p)
	}
	return result
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// buildComponentBlock parses a ```component fence body into a ContentBlock.
// Expected JSON: {"type": "data-card", "title": "...", "metrics": [...], ...}
// The "type" field becomes ComponentType; everything else becomes Props.
func buildComponentBlock(body string) *ContentBlock {
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil
	}
	compType := strVal(raw, "type")
	if compType == "" {
		return nil
	}
	delete(raw, "type")
	return &ContentBlock{
		Type:          "component",
		ComponentType: compType,
		Props:         raw,
	}
}
