package exec

import (
	"fmt"
	"strings"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

// materializeSubscriptionStepSQL rebuilds executable subscription SQL from
// desired Subscription IR. Connection-bearing steps never execute redacted
// Step.SQL; missing plaintext IR fails closed with a clear error.
func materializeSubscriptionStepSQL(step plan.Step, subs map[string]*schema.Subscription) (string, error) {
	switch step.Type {
	case plan.ChangeCreateSubscription:
		sub := lookupDesiredSubscription(subs, step.Table)
		if !schema.SubscriptionHasUsableConnInfo(sub) {
			return "", fmt.Errorf("cannot apply CONNECTION for subscription %q: plaintext conninfo IR is unavailable; provide CatalogSQL/SchemaSQL with the real CONNECTION (plan artifacts redact secrets and cannot be applied alone)", step.Table)
		}
		return postgres.GenerateCreateSubscriptionApplySQL(sub) + "\n" + postgres.GenerateSubscriptionCommentSQL(sub.Name), nil
	case plan.ChangeAlterSubscription:
		if !strings.Contains(step.SQL, "CONNECTION") {
			return step.SQL, nil
		}
		sub := lookupDesiredSubscription(subs, step.Table)
		if !schema.SubscriptionHasUsableConnInfo(sub) {
			return "", fmt.Errorf("cannot apply CONNECTION for subscription %q: plaintext conninfo IR is unavailable; provide CatalogSQL/SchemaSQL with the real CONNECTION (plan artifacts redact secrets and cannot be applied alone)", step.Table)
		}
		// Step.SQL was rendered with the redaction placeholder; splice the
		// plaintext conninfo back in for apply without re-deriving drift.
		redactedLit := "'" + schema.PasswordRedacted + "'"
		if !strings.Contains(step.SQL, redactedLit) {
			return step.SQL, nil
		}
		return strings.ReplaceAll(step.SQL, redactedLit, "'"+escapeSQLLiteral(sub.ConnInfo)+"'"), nil
	default:
		return step.SQL, nil
	}
}

func lookupDesiredSubscription(subs map[string]*schema.Subscription, name string) *schema.Subscription {
	if subs == nil {
		return nil
	}
	if sub := subs[schema.CanonicalIdentifierKey(name)]; sub != nil {
		return sub
	}
	return subs[name]
}

func subscriptionIRFromConfig(cfg PostgresExecConfig) map[string]*schema.Subscription {
	desired, err := desiredCatalogSpec(cfg)
	if err != nil || desired == nil {
		return nil
	}
	out := make(map[string]*schema.Subscription, len(desired.Subscriptions))
	for _, sub := range desired.Subscriptions {
		out[schema.CanonicalIdentifierKey(sub.Name)] = sub
	}
	return out
}

func escapeSQLLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
