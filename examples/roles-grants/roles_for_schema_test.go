package main

import "testing"

func TestRolesForTarget(t *testing.T) {
	const roles = "GRANT USAGE ON SCHEMA public TO grizzle_example_read;\nGRANT CONNECT ON DATABASE app TO grizzle_example_read;\nGRANT SELECT ON docs TO grizzle_example_read;\n"

	if got := rolesForTarget(roles, "", "app"); got != roles {
		t.Fatalf("defaults must keep roles unchanged, got %q", got)
	}
	if got := rolesForTarget(roles, "public", "app"); got != roles {
		t.Fatalf("public target on app must keep roles unchanged, got %q", got)
	}

	want := "GRANT USAGE ON SCHEMA \"test_example\" TO grizzle_example_read;\nGRANT CONNECT ON DATABASE \"grizzle_test\" TO grizzle_example_read;\nGRANT SELECT ON docs TO grizzle_example_read;\n"
	if got := rolesForTarget(roles, "test_example", "grizzle_test"); got != want {
		t.Fatalf("retarget mismatch:\n got %q\nwant %q", got, want)
	}

	if got := quoteIdent(`we"ird`); got != `"we""ird"` {
		t.Fatalf("quoteIdent mismatch, got %q", got)
	}
}
