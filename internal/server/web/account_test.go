package web

import (
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// TestClassifyInviteConsistency pins that an invite's status label and its
// revocability derive from one instant, so the two can never disagree: a redeemed
// invite is non-revocable, an expired one is non-revocable and reads "expired", and an
// invite whose expiry sits exactly at now is classified once (both projections agree)
// rather than flipping between two separate time.Now() reads.
func TestClassifyInviteConsistency(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	redeemedAt := now.Add(-time.Minute)
	redeemer := "ada"

	// Redeemed: keyed on RedeemedAt, status names the redeemer, not revocable.
	redeemed := classifyInvite(store.Invite{RedeemedAt: &redeemedAt, RedeemedBy: &redeemer}, now)
	if redeemed.Status != "redeemed by ada" || redeemed.Revocable {
		t.Errorf("redeemed = %+v, want 'redeemed by ada' and not revocable", redeemed)
	}

	// Redeemed but RedeemedBy not yet patched (redeemed_at set, redeemed_by NULL): still
	// reads as redeemed and non-revocable, keying on RedeemedAt like the store does,
	// rather than showing "unused" and a Revoke button for a row the store considers gone.
	midRedeem := classifyInvite(store.Invite{RedeemedAt: &redeemedAt}, now)
	if midRedeem.Status != "redeemed" || midRedeem.Revocable {
		t.Errorf("redeemed-at-only = %+v, want 'redeemed' and not revocable (keyed on RedeemedAt)", midRedeem)
	}

	// Expired: past its expiry, reads "expired", not revocable.
	past := now.Add(-time.Hour)
	expired := classifyInvite(store.Invite{ExpiresAt: &past}, now)
	if expired.Status != "expired" || expired.Revocable {
		t.Errorf("expired = %+v, want 'expired' and not revocable", expired)
	}

	// Open with a future expiry: unused and revocable.
	future := now.Add(time.Hour)
	open := classifyInvite(store.Invite{ExpiresAt: &future}, now)
	if open.Status != "unused" || !open.Revocable {
		t.Errorf("open = %+v, want 'unused' and revocable", open)
	}

	// Expiry exactly at now: the store redeems only while expires_at > now, so an
	// invite whose expiry equals now is already unredeemable. It must read "expired"
	// and be non-revocable, matching the store's boundary rather than showing a Revoke
	// button for a token no one can accept.
	boundary := classifyInvite(store.Invite{ExpiresAt: &now}, now)
	if boundary.Status != "expired" || boundary.Revocable {
		t.Errorf("expiry == now = %+v, want 'expired' and not revocable (matches expires_at > now)", boundary)
	}

	// No expiry: unused and revocable indefinitely.
	noExpiry := classifyInvite(store.Invite{}, now)
	if noExpiry.Status != "unused" || !noExpiry.Revocable {
		t.Errorf("no-expiry = %+v, want 'unused' and revocable", noExpiry)
	}

	// The label and the button are the same projection: an invite is revocable if and
	// only if its status is "unused", for every classification above. This is the
	// invariant a two-now split could break.
	for _, v := range []inviteView{redeemed, midRedeem, expired, open, boundary, noExpiry} {
		if v.Revocable != (v.Status == "unused") {
			t.Errorf("state %+v breaks the revocable-iff-unused invariant", v)
		}
	}
}
