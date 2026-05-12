module github.com/carbon-os/diskwim/cmd/diskwim

go 1.25.7

replace github.com/carbon-os/diskwim => ../../

require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/carbon-os/diskiso v0.0.0-20260512032318-28f69c98239d
	github.com/carbon-os/diskwim v0.0.0-00010101000000-000000000000
)
