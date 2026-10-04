package sessions

import "github.com/rcpassos/mergeyard/internal/fault"

// Session errors use phase.* codes. Causes remain available to errors.Is/As.
func failure(code, path string, err error) error {
	if err == nil {
		return nil
	}
	return &fault.Error{Code: code, Path: path, Err: err}
}
