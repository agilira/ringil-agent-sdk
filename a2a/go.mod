module github.com/agilira/ringil-agent-sdk/a2a

go 1.27.0

require (
	github.com/a2aproject/a2a-go/v2 v2.8.0
	github.com/agilira/go-errors v1.1.3
	github.com/agilira/ringil-agent-sdk v0.0.0
)

require (
	github.com/agilira/go-timecache v1.0.4 // indirect
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

// Until the library has a tag, this module builds against the code next to it.
replace github.com/agilira/ringil-agent-sdk => ..
