package exec

import (
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

func TestMaterializeSubscriptionStepSQL(t *testing.T) {
	subs := map[string]*schema.Subscription{
		"sub_a": {
			Name:         "sub_a",
			ConnInfo:     "host=db password=s3cret",
			Publications: []string{"p"},
			Enabled:      true,
			CopyData:     true,
			SlotName:     "sub_a",
		},
	}
	sql, err := materializeSubscriptionStepSQL(plan.Step{
		Type:  plan.ChangeCreateSubscription,
		Table: "sub_a",
		SQL:   "CREATE SUBSCRIPTION \"sub_a\" CONNECTION '" + schema.PasswordRedacted + "' PUBLICATION \"p\";",
	}, subs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, schema.PasswordRedacted) || !strings.Contains(sql, "s3cret") {
		t.Fatalf("expected plaintext apply SQL: %s", sql)
	}

	_, err = materializeSubscriptionStepSQL(plan.Step{
		Type:  plan.ChangeCreateSubscription,
		Table: "sub_a",
		SQL:   "CREATE ...",
	}, map[string]*schema.Subscription{
		"sub_a": {Name: "sub_a", ConnInfo: schema.PasswordRedacted},
	})
	if err == nil {
		t.Fatal("expected refuse when IR only has redacted conninfo")
	}
}
