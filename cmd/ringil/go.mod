module github.com/agilira/ringil-agent-sdk/cmd/ringil

go 1.27.0

require (
	github.com/agilira/go-errors v1.1.3
	github.com/agilira/orpheus v1.6.0
	github.com/agilira/ringil-agent-sdk v0.0.0
	github.com/agilira/ringil-agent-sdk/a2a v0.0.0
	github.com/agilira/ringil-agent-sdk/mcp v0.0.0
)

require (
	github.com/a2aproject/a2a-go/v2 v2.8.0 // indirect
	github.com/agilira/flash-flags v1.3.1 // indirect
	github.com/agilira/go-timecache v1.0.4 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/modelcontextprotocol/go-sdk v1.8.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	mvdan.cc/sh/v3 v3.14.1 // indirect
)

// Until the library has a tag, the CLI builds against the code next to it.
replace github.com/agilira/ringil-agent-sdk => ../..

replace github.com/agilira/ringil-agent-sdk/mcp => ../../mcp

replace github.com/agilira/ringil-agent-sdk/a2a => ../../a2a
