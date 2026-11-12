package lsp

import "fmt"

// document is the editor's content of an open file.
type document struct {
	uri     string
	path    string
	version int
	text    []byte
}

// contentChange is one entry of didChange's contentChanges: a replacement of
// range, or of the whole text when range is absent.
type contentChange struct {
	Range *Range `json:"range,omitempty"`
	Text  string `json:"text"`
}

// apply replaces the text in order of changes; positions of each change
// refer to the text after the previous one.
func (d *document) apply(changes []contentChange, enc encoding) error {
	for _, c := range changes {
		if c.Range == nil {
			d.text = []byte(c.Text)
			continue
		}
		start := offsetOf(d.text, c.Range.Start, enc)
		end := offsetOf(d.text, c.Range.End, enc)
		if end < start {
			return fmt.Errorf("change range ends before it starts")
		}
		next := make([]byte, 0, len(d.text)-(end-start)+len(c.Text))
		next = append(next, d.text[:start]...)
		next = append(next, c.Text...)
		next = append(next, d.text[end:]...)
		d.text = next
	}
	return nil
}
