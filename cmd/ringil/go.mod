module github.com/agilira/ringil-agent-sdk/cmd/ringil

go 1.27.0

require (
	github.com/agilira/go-errors v1.1.3
	github.com/agilira/orpheus v1.6.0
	github.com/agilira/ringil-agent-sdk v0.0.0
)

require (
	github.com/agilira/flash-flags v1.3.1 // indirect
	github.com/agilira/go-timecache v1.0.4 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/term v0.43.0 // indirect
)

// Until the library has a tag, the CLI builds against the code next to it.
replace github.com/agilira/ringil-agent-sdk => ../..
