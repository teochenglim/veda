package telemetry

import (
	"encoding/json"
	"runtime"
)

func osName() string   { return runtime.GOOS }
func archName() string { return runtime.GOARCH }

func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

func jsonMarshal(v any) ([]byte, error)      { return json.Marshal(v) }
func jsonUnmarshal(data string, v any) error { return json.Unmarshal([]byte(data), v) }
