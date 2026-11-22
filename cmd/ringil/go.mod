module github.com/agilira/ringil-agent-sdk/cmd/ringil

go 1.27.0

require (
	github.com/agilira/argus v1.8.1
	github.com/agilira/go-errors v1.1.3
	github.com/agilira/orpheus v1.6.1
	github.com/agilira/ringil-agent-sdk v0.0.0
	github.com/agilira/ringil-agent-sdk/a2a v0.0.0
	github.com/agilira/ringil-agent-sdk/mcp v0.0.0
	github.com/agilira/ringil-agent-sdk/memory/sqlite v0.0.0
	github.com/agilira/ringil-agent-sdk/provider/anthropic v0.0.0
	github.com/agilira/ringil-agent-sdk/secrets/keyring v0.0.0
	golang.org/x/term v0.48.0
)

require (
	github.com/a2aproject/a2a-go/v2 v2.8.0 // indirect
	github.com/agilira/flash-flags v1.3.2 // indirect
	github.com/agilira/go-timecache v1.0.4 // indirect
	github.com/anthropics/anthropic-sdk-go/v2 v2.13.0 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-sqlite3 v1.14.53 // indirect
	github.com/modelcontextprotocol/go-sdk v1.8.0 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.1 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/otel v1.33.0 // indirect
	go.opentelemetry.io/otel/metric v1.33.0 // indirect
	go.opentelemetry.io/otel/trace v1.33.0 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.61.1 // indirect
	mvdan.cc/sh/v3 v3.14.1 // indirect
)

// Until the library has a tag, the CLI builds against the code next to it.
replace github.com/agilira/ringil-agent-sdk => ../..

replace github.com/agilira/ringil-agent-sdk/mcp => ../../mcp

replace github.com/agilira/ringil-agent-sdk/a2a => ../../a2a

replace github.com/agilira/ringil-agent-sdk/memory/sqlite => ../../memory/sqlite

replace github.com/agilira/ringil-agent-sdk/provider/anthropic => ../../provider/anthropic

replace github.com/agilira/ringil-agent-sdk/secrets/keyring => ../../secrets/keyring
