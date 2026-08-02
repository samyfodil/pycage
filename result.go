package pycage

// Output is a rich value produced by the final expression in a code cell.
type Output struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// ExecutionError describes a Python exception without turning it into a host
// transport error.
type ExecutionError struct {
	Name      string `json:"name"`
	Message   string `json:"message"`
	Traceback string `json:"traceback"`
}

// Execution contains the captured effects of one Python cell.
type Execution struct {
	Outputs []Output        `json:"outputs"`
	Stdout  string          `json:"stdout"`
	Stderr  string          `json:"stderr"`
	Error   *ExecutionError `json:"error"`
}

// Text returns the first text output, or an empty string when the cell did not
// evaluate to a displayable value.
func (e Execution) Text() string {
	for _, output := range e.Outputs {
		if output.Type == "text" {
			if text, ok := output.Data.(string); ok {
				return text
			}
		}
	}
	return ""
}
