package postgres

import (
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/schema"
)

func TestGenerateSubscriptionSQL_RedactedAndDropKeepsRemoteSlot(t *testing.T) {
	sub := &schema.Subscription{
		Name:         "sub_a",
		ConnInfo:     "host=db password=s3cret",
		Publications: []string{"pub1", "pub2"},
		Enabled:      false,
		CopyData:     false,
		SlotName:     "custom_slot",
	}
	create := GenerateCreateSubscriptionSQL(sub)
	if strings.Contains(create, "s3cret") {
		t.Fatalf("create SQL leaked conninfo: %s", create)
	}
	if !strings.Contains(create, schema.PasswordRedacted) {
		t.Fatalf("expected redacted conninfo: %s", create)
	}
	if !strings.Contains(create, "enabled = false") || !strings.Contains(create, "copy_data = false") {
		t.Fatalf("expected WITH options: %s", create)
	}
	apply := GenerateCreateSubscriptionApplySQL(sub)
	if !strings.Contains(apply, "s3cret") {
		t.Fatalf("apply SQL must carry plaintext conninfo: %s", apply)
	}
	drop := GenerateDropSubscriptionSQL("sub_a")
	if !strings.Contains(drop, "SET (slot_name = NONE)") || !strings.Contains(drop, "DISABLE") {
		t.Fatalf("drop must keep remote slot: %s", drop)
	}
	comment := GenerateSubscriptionCommentSQL("sub_a")
	if !strings.Contains(comment, schema.SubscriptionManagedComment) {
		t.Fatalf("missing managed comment: %s", comment)
	}
}

func TestGenerateReplicationSlotSQL(t *testing.T) {
	create := GenerateCreateReplicationSlotSQL(&schema.ReplicationSlot{Name: "s", Plugin: "pgoutput"})
	if create != "SELECT pg_create_logical_replication_slot('s', 'pgoutput');" {
		t.Fatalf("unexpected create: %s", create)
	}
	tmp := GenerateCreateReplicationSlotSQL(&schema.ReplicationSlot{Name: "s", Plugin: "pgoutput", Temporary: true})
	if !strings.Contains(tmp, ", true)") {
		t.Fatalf("temporary slot: %s", tmp)
	}
	drop := GenerateDropReplicationSlotSQL("s")
	if drop != "SELECT pg_drop_replication_slot('s');" {
		t.Fatalf("unexpected drop: %s", drop)
	}
}
