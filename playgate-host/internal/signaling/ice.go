package signaling

import "encoding/json"

// URLs normalises the ICEServer.URLs field, which the Worker may encode as either
// a single JSON string or an array of strings, into a []string. Unparseable
// input yields nil.
func (s ICEServer) URLList() []string {
	if len(s.URLs) == 0 {
		return nil
	}
	// Try array of strings first.
	var arr []string
	if err := json.Unmarshal(s.URLs, &arr); err == nil {
		return arr
	}
	// Fall back to a single string.
	var single string
	if err := json.Unmarshal(s.URLs, &single); err == nil && single != "" {
		return []string{single}
	}
	return nil
}
