package schema

import (
	"strings"
	"testing"
)

func TestParseCatalogSQL_SubscriptionsAndSlots(t *testing.T) {
	sql := `
CREATE SUBSCRIPTION sub_a CONNECTION 'host=db password=s3cret' PUBLICATION pub1, pub2 WITH (copy_data = false, enabled = false, slot_name = custom_slot);
ALTER SUBSCRIPTION sub_a CONNECTION 'host=db password=new';
ALTER SUBSCRIPTION sub_a ENABLE;
ALTER SUBSCRIPTION sub_a SET PUBLICATION pub2, pub3;
SELECT pg_create_logical_replication_slot('slot_a', 'pgoutput');
SELECT pg_create_logical_replication_slot('tmp_slot', 'pgoutput', true);
SELECT pg_drop_replication_slot('old_slot');
DROP SUBSCRIPTION gone_sub;
`
	if err := ValidateCatalogSQL(sql); err != nil {
		t.Fatalf("ValidateCatalogSQL: %v", err)
	}
	spec := ParseCatalogSQL(sql)
	if len(spec.Subscriptions) != 1 {
		t.Fatalf("expected 1 subscription after alters, got %d", len(spec.Subscriptions))
	}
	sub := spec.Subscriptions[0]
	if sub.Name != "sub_a" || sub.ConnInfo != "host=db password=new" || !sub.Enabled || sub.CopyData {
		t.Fatalf("unexpected subscription state: %+v", sub)
	}
	if sub.SlotName != "custom_slot" {
		t.Fatalf("slot_name = %q, want custom_slot", sub.SlotName)
	}
	if strings.Join(sub.Publications, ",") != "pub2,pub3" {
		t.Fatalf("publications = %v", sub.Publications)
	}
	if !spec.SubscriptionExplicitlyDropped("gone_sub") {
		t.Fatal("expected gone_sub explicitly dropped")
	}
	if len(spec.ReplicationSlots) != 2 {
		t.Fatalf("expected 2 slots, got %d", len(spec.ReplicationSlots))
	}
	if !spec.ReplicationSlotExplicitlyDropped("old_slot") {
		t.Fatal("expected old_slot explicitly dropped")
	}
	foundTmp := false
	for _, slot := range spec.ReplicationSlots {
		if slot.Name == "tmp_slot" {
			foundTmp = true
			if !slot.Temporary || slot.Plugin != "pgoutput" {
				t.Fatalf("tmp_slot = %+v", slot)
			}
		}
	}
	if !foundTmp {
		t.Fatal("tmp_slot missing")
	}
}

func TestParseCatalogSQL_SubscriptionDefaults(t *testing.T) {
	spec := ParseCatalogSQL(`CREATE SUBSCRIPTION s CONNECTION 'dbname=x' PUBLICATION p;`)
	if len(spec.Subscriptions) != 1 {
		t.Fatal("expected subscription")
	}
	sub := spec.Subscriptions[0]
	if !sub.Enabled || !sub.CopyData || sub.SlotName != "s" {
		t.Fatalf("defaults not applied: %+v", sub)
	}
}

func TestValidateCatalogSQL_RefuseUnsupported(t *testing.T) {
	cases := []string{
		`CREATE SUBSCRIPTION s CONNECTION 'x' PUBLICATION p WITH (binary = true);`,
		`SELECT pg_create_physical_replication_slot('p');`,
		`ALTER SUBSCRIPTION s REFRESH PUBLICATION;`,
		`SELECT 1;`,
	}
	for _, sql := range cases {
		if err := ValidateCatalogSQL(sql); err == nil {
			t.Fatalf("expected error for %q", sql)
		}
	}
}

func TestRedactSubscriptionConnInfoSQL(t *testing.T) {
	in := `CREATE SUBSCRIPTION s CONNECTION 'host=db password=s3cret' PUBLICATION p;
ALTER SUBSCRIPTION s CONNECTION 'host=db password=new';`
	out := RedactSubscriptionConnInfoSQL(in)
	if strings.Contains(out, "s3cret") || strings.Contains(out, "password=new") {
		t.Fatalf("secret leaked: %s", out)
	}
	if !strings.Contains(out, "'"+PasswordRedacted+"'") {
		t.Fatalf("expected redaction placeholder: %s", out)
	}
}

func TestExtractStatements_SubscriptionsAndSlots(t *testing.T) {
	sql := `CREATE TABLE t (id int);
CREATE SUBSCRIPTION s CONNECTION 'host=x' PUBLICATION p;
SELECT pg_create_logical_replication_slot('slot1', 'pgoutput');
ALTER TABLE t ADD COLUMN n int;`
	groups := ExtractStatements(sql)
	if strings.Contains(groups.ShadowSQL, "SUBSCRIPTION") ||
		strings.Contains(groups.ShadowSQL, "pg_create_logical_replication_slot") {
		t.Fatalf("catalog statements leaked into shadow: %q", groups.ShadowSQL)
	}
	if !strings.Contains(groups.CatalogSQL, "CREATE SUBSCRIPTION") ||
		!strings.Contains(groups.CatalogSQL, "pg_create_logical_replication_slot") {
		t.Fatalf("catalog statements not extracted: %q", groups.CatalogSQL)
	}
	if !strings.Contains(groups.ShadowSQL, "CREATE TABLE") {
		t.Fatalf("shadow lost DDL: %q", groups.ShadowSQL)
	}
}
