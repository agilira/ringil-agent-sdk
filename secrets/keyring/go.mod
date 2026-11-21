module github.com/agilira/ringil-agent-sdk/secrets/keyring

go 1.27.0

require (
	github.com/agilira/go-errors v1.1.3
	github.com/agilira/ringil-agent-sdk v0.0.0
	github.com/danieljoos/wincred v1.2.3
	github.com/godbus/dbus/v5 v5.2.2
)

require (
	github.com/agilira/go-timecache v1.0.4 // indirect
	golang.org/x/sys v0.50.0 // indirect
)

replace github.com/agilira/ringil-agent-sdk => ../..
