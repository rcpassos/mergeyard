package web

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Read only the bounded end of a harness event stream inside this workspace.
// Root prevents persisted paths or symlinks from exposing unrelated host files.
func logTail(workspace, path string) (string, string) {
	if path == "" {
		return "", "No phase output yet."
	}
	relative, err := filepath.Rel(workspace, path)
	if err != nil || !strings.HasPrefix(relative, "runs"+string(filepath.Separator)) || filepath.Base(path) != "events.jsonl" {
		return "", "Phase output is outside the managed runs directory."
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", "Could not open phase output."
	}
	defer root.Close()
	file, err := root.Open(relative)
	if err != nil {
		return "", "Phase output is not available yet."
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return "", "Could not read phase output."
	}
	const window = 128 << 10
	start := max(int64(0), stat.Size()-window)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", "Could not read phase output."
	}
	data, err := io.ReadAll(io.LimitReader(file, window))
	if err != nil {
		return "", "Could not read phase output."
	}
	if start > 0 {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		} else {
			data = nil
		}
	}
	var output strings.Builder
	lines := bytes.Split(data, []byte{'\n'})
	for _, line := range lines[max(0, len(lines)-200):] {
		var event struct {
			Type    string `json:"type"`
			Result  string `json:"result"`
			Message struct {
				Content []struct{ Type, Text, Name string }
			} `json:"message"`
			Event            struct{ Delta struct{ Text string } } `json:"event"`
			Item             struct{ Type, Text string }           `json:"item"`
			StructuredOutput struct {
				Summary string `json:"summary"`
			} `json:"structured_output"`
		}
		// Incomplete lines and non-event data are ignored, never inferred as state.
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		switch event.Type {
		case "assistant":
			for _, content := range event.Message.Content {
				if content.Type == "text" {
					output.WriteString(content.Text + "\n")
				}
				if content.Type == "tool_use" {
					output.WriteString("Tool: " + content.Name + "\n")
				}
			}
		case "stream_event":
			output.WriteString(event.Event.Delta.Text)
		case "result":
			text := event.StructuredOutput.Summary
			if text == "" {
				text = event.Result
			}
			if text != "" {
				output.WriteString(text + "\n")
			}
		case "item.completed":
			if event.Item.Type == "agent_message" {
				output.WriteString(event.Item.Text + "\n")
			}
		}
	}
	text := output.String()
	if len(text) > 32<<10 {
		text = strings.ToValidUTF8(text[len(text)-(32<<10):], "")
	}
	return text, "Showing up to 200 recent events from the last 128 KiB; rendered text is limited to 32 KiB."
}
