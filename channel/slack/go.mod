module github.com/agilira/ringil-agent-sdk/channel/slack

go 1.27.0

require (
	github.com/agilira/go-errors v1.1.3
	github.com/agilira/ringil-agent-sdk v0.0.0
	github.com/coder/websocket v1.8.15
)

require (
	github.com/agilira/go-timecache v1.0.4 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.44.0 // indirect
)

replace github.com/agilira/ringil-agent-sdk => ../..
