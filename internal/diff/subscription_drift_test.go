package diff

import (
	"testing"

	"github.com/muandane/grizzle/internal/schema"
)

// A redacted desired conninfo cannot be compared with live plaintext; it must
// not be reported as drift (apply refuses redacted-only CONNECTION anyway).
func TestSubscriptionHasDrift_RedactedConnInfoIsNotDrift(t *testing.T) {
	want := &schema.Subscription{
		Name:         "sub_a",
		ConnInfo:     schema.PasswordRedacted,
		Publications: []string{"pub1"},
		Enabled:      true,
	}
	live := &SubscriptionState{
		Name:         "sub_a",
		ConnInfo:     "host=db password=s3cret",
		Publications: []string{"pub1"},
		Enabled:      true,
	}
	if subscriptionHasDrift(want, live) {
		t.Fatal("redacted desired conninfo must not produce conninfo drift")
	}
}

func TestSubscriptionHasDrift_PlaintextConnInfoChangeIsDrift(t *testing.T) {
	want := &schema.Subscription{
		Name:         "sub_a",
		ConnInfo:     "host=new password=s3cret",
		Publications: []string{"pub1"},
		Enabled:      true,
	}
	live := &SubscriptionState{
		Name:         "sub_a",
		ConnInfo:     "host=old password=s3cret",
		Publications: []string{"pub1"},
		Enabled:      true,
	}
	if !subscriptionHasDrift(want, live) {
		t.Fatal("usable plaintext conninfo change must be reported as drift")
	}
}
