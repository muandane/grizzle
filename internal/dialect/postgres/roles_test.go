package postgres

import "testing"

func TestCanonicalLiveGrantObject_FunctionSignature(t *testing.T) {
	tests := []struct {
		schema, name, args, want string
	}{
		{schema: "public", name: "touch_ts", args: "", want: "public.touch_ts()"},
		{schema: "Public", name: "Touch_TS", args: "integer, text", want: `"Public"."Touch_TS"(integer, text)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalLiveGrantObject("FUNCTION", tt.schema+"."+tt.name+"("+tt.args+")")
			if got != tt.want {
				t.Fatalf("canonicalLiveGrantObject = %q, want %q", got, tt.want)
			}
		})
	}
}
