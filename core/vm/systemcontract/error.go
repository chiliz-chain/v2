package systemcontract

import "fmt"

var (
	errNotSupported   = fmt.Errorf("not supported")
	errMethodNotFound = fmt.Errorf("method not found")
	errInvalidCaller  = fmt.Errorf("invalid caller")
	errFailedToUnpack = fmt.Errorf("failed to unpack")
)
