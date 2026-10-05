# Fix fixture issue 37
Repository: example.invalid/probe. Base branch: main. This is the complete fix input.
R1-F1: Value() must return 3. Change the implementation and expected test result to 3. Run go test ./... and go build ./... using GOCACHE="$PWD/.cache/go-build". Return the fix contract with R1-F1 fixed. Include the conversation-only marker from the original implement turn in summary; it is deliberately absent from this input. Include the configured skill marker. Keep changes uncommitted.
