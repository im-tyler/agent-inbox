package inbox

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// LastAssistantText returns the concatenated text of the last assistant turn
// in a Claude Code transcript (JSONL). Schema verified against claude 2.1:
// lines are {type, message:{role, content:[{type:"text", text}]}}.
func LastAssistantText(transcriptPath string) string {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var d struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &d) != nil || d.Type != "assistant" {
			continue
		}
		var sb strings.Builder
		for _, c := range d.Message.Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		if t := strings.TrimSpace(sb.String()); t != "" {
			last = t
		}
	}
	return last
}
