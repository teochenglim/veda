package telemetry

import (
	"encoding/json"
	"runtime"
)

func osName() string   { return runtime.GOOS }
func archName() string { return runtime.GOARCH }

func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
