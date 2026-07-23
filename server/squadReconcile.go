package server

import (
	"ChromehoundsStatusServer/logging"
	"context"
	"fmt"
	"sort"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Squad-consistency reconciliation. The registration (181) and join (182) paths link a player's profile to
// a new squad WITHOUT removing them from a prior one (no single-squad invariant), so a player accumulates
// membership in every squad they ever created/joined. This reconciler restores "one squad per player":
// for each player in >1 squad it keeps exactly one and removes them from the rest, then repairs every
// profile.teamId to match actual membership. Dry-run unless applied.

// --- pure planner (unit-tested; no I/O) ---

// squadMembership is one place a player is currently a member.
type squadMembership struct {
	TeamID string
	Leader bool
	Size   int // member count of that squad (to tell a solo squad from one with followers)
}

// memberDisposition is what happens to one of a player's memberships.
//
//	keep    - the squad the player stays in
//	pull    - remove the player (a non-leader) from this squad
//	disband - delete this squad (the player is its SOLE member/leader -> an orphaned solo squad)
//	flag    - the player LEADS this squad and it has other members, yet it is not the keep: NOT
//	          auto-touched (removing a leader would orphan the followers). Since a led-with-followers
//	          squad wins the keep choice, this only remains for a player leading SEVERAL such squads
//	          -> reported for a manual leader-reassignment decision on the extras
type memberDisposition struct {
	TeamID string
	Kind   string
}

type playerPlan struct {
	XUID         string
	Keep         string
	Dispositions []memberDisposition
}

// chooseKeepSquad picks the squad a multi-squad player should remain in:
//  1. a squad they LEAD that has followers — "leader with followers wins" (operator policy 2026-07-17):
//     the leader can't be pulled without orphaning the followers, so leadership outranks the profile
//     pointer. If they lead several such squads, the profile pointer breaks the tie when it is one of
//     them, else the most recent wins; the rest still get flagged (a followed leader is never pulled).
//  2. else profileTeam, if the player is actually a member of it (the app's own "current squad" pointer);
//  3. else the single squad they LEAD, if exactly one (necessarily solo-led, given rule 1);
//  4. else the most-recent squad (highest teamId; ids are zero-padded, so lexical order == chronological).
func chooseKeepSquad(ms []squadMembership, profileTeam string) string {
	inProfile := false
	var leaderTeams, ledWithFollowers []string
	maxTeam := ""
	for _, m := range ms {
		if m.TeamID == profileTeam {
			inProfile = true
		}
		if m.Leader {
			leaderTeams = append(leaderTeams, m.TeamID)
			if m.Size > 1 {
				ledWithFollowers = append(ledWithFollowers, m.TeamID)
			}
		}
		if m.TeamID > maxTeam {
			maxTeam = m.TeamID
		}
	}
	if len(ledWithFollowers) > 0 {
		best := ""
		for _, t := range ledWithFollowers {
			if t == profileTeam {
				return t
			}
			if t > best {
				best = t
			}
		}
		return best
	}
	if inProfile {
		return profileTeam
	}
	if len(leaderTeams) == 1 {
		return leaderTeams[0]
	}
	return maxTeam
}

// planPlayer computes the disposition of every membership for a player in >1 squad.
func planPlayer(xuid string, ms []squadMembership, profileTeam string) playerPlan {
	// deterministic order so the report + apply are stable.
	sorted := append([]squadMembership(nil), ms...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TeamID < sorted[j].TeamID })

	keep := chooseKeepSquad(sorted, profileTeam)
	p := playerPlan{XUID: xuid, Keep: keep}
	for _, m := range sorted {
		switch {
		case m.TeamID == keep:
			p.Dispositions = append(p.Dispositions, memberDisposition{m.TeamID, "keep"})
		case !m.Leader:
			p.Dispositions = append(p.Dispositions, memberDisposition{m.TeamID, "pull"})
		case m.Size <= 1:
			p.Dispositions = append(p.Dispositions, memberDisposition{m.TeamID, "disband"})
		default:
			p.Dispositions = append(p.Dispositions, memberDisposition{m.TeamID, "flag"})
		}
	}
	return p
}

// leaveOtherSquads enforces the single-squad invariant for a player who just created/joined keepTeamID: it
// removes them from every OTHER squad they are still in, so membership can't accumulate the way it did
// before this guard (registration/join used to repoint profile.teamId without unlinking the prior squad).
// Non-leaders are pulled; a squad the player SOLELY occupies is disbanded; a squad they LEAD that has other
// members is left intact and logged (removing the leader would orphan the followers — a manual reconcile
// case, matching the reconciler's `flag`). Best-effort: a failure is logged, never fatal to the create/join
// that triggered it (mirrors the other profile-unlink side effects). AI/CPU squads are skipped upstream.
func (r *SquadRepository) leaveOtherSquads(ctx context.Context, xuid, keepTeamID string) {
	if xuid == "" {
		return
	}
	var squads []Squad
	if cur, err := r.squads.Find(ctx, bson.M{"members.xuid": xuid, "teamId": bson.M{"$ne": keepTeamID}}); err != nil {
		logging.Warn.Printf("[squad] leaveOtherSquads find failed for %s: %v", xuid, err)
		return
	} else if err := cur.All(ctx, &squads); err != nil {
		logging.Warn.Printf("[squad] leaveOtherSquads decode failed for %s: %v", xuid, err)
		return
	}
	for _, sq := range squads {
		leader := false
		for _, m := range sq.Members {
			if m.XUID == xuid {
				leader = m.Leader
				break
			}
		}
		switch {
		case len(sq.Members) <= 1:
			// The player is the only member -> the squad is orphaned by their move; disband it.
			if _, err := r.squads.DeleteOne(ctx, bson.M{"teamId": sq.TeamID}); err != nil {
				logging.Warn.Printf("[squad] leaveOtherSquads disband %s failed: %v", sq.TeamID, err)
			}
		case leader:
			// Leader with followers: don't orphan them. Legitimate play requires disbanding first, so this
			// only happens on anomalous state -> leave it for `cmd/reconcile-squads` to surface.
			logging.Warn.Printf("[squad] %s created/joined %s but still leads %s (has followers); not auto-removed — run reconcile-squads", xuid, keepTeamID, sq.TeamID)
		default:
			if _, err := r.squads.UpdateOne(ctx, bson.M{"teamId": sq.TeamID}, bson.M{"$pull": bson.M{"members": bson.M{"xuid": xuid}}, "$inc": bumpSeq}); err != nil {
				logging.Warn.Printf("[squad] leaveOtherSquads pull %s from %s failed: %v", xuid, sq.TeamID, err)
			}
		}
	}
}

// RefreshGamertag corrects a player's gamertag from a RELIABLE self-report — their OWN squad-reg (181) or
// squad-login (184), where parts[0] is the caller's own tag. This repairs the mis-sourced gamertag the host
// bakes in during a 182 join: the host sources the join body's gamertag from the squad lead / a prior
// member, so members land in the DB with someone else's tag, and EnsureProfile (which only sets gamertag on
// insert) then freezes that wrong tag onto a newly-created profile. Updates the persistent profile AND the
// player's roster entry in whatever squad currently lists them. No-op on an empty gamertag or when already
// current (guarded by $ne), and best-effort — never fatal to the reg/login it rides on.
func (r *SquadRepository) RefreshGamertag(ctx context.Context, xuid, gamertag string) {
	if xuid == "" || gamertag == "" {
		return
	}
	if _, err := r.profiles.UpdateOne(ctx,
		bson.M{"xuid": xuid, "gamertag": bson.M{"$ne": gamertag}},
		bson.M{"$set": bson.M{"gamertag": gamertag}},
	); err != nil {
		logging.Warn.Printf("[squad] RefreshGamertag profile %s failed: %v", xuid, err)
	}
	// $elemMatch so the positional $ targets the caller's own roster entry (and only when it differs).
	if _, err := r.squads.UpdateOne(ctx,
		bson.M{"members": bson.M{"$elemMatch": bson.M{"xuid": xuid, "gamertag": bson.M{"$ne": gamertag}}}},
		bson.M{"$set": bson.M{"members.$.gamertag": gamertag}, "$inc": bumpSeq},
	); err != nil {
		logging.Warn.Printf("[squad] RefreshGamertag member %s failed: %v", xuid, err)
	}
}

// --- orchestration (I/O) ---

// ReconcileReport summarises what the reconciler did (or would do on a dry run).
type ReconcileReport struct {
	Applied           bool
	SquadsScanned     int
	MultiSquadPlayers int
	Pulls             []string // "<xuid> pulled from <teamId>"
	Disbands          []string // "<teamId> disbanded (orphaned solo squad led by <xuid>)"
	Flags             []string // "<xuid> leads <teamId> (N members) but home is <keep> — MANUAL review"
	ProfileFixes      []string // "<xuid> profile.teamId <old> -> <new>"
}

// ReconcileSquads restores the one-squad-per-player invariant. apply=false is a dry run (computes + reports
// the plan, writes nothing). It also repairs every profile.teamId to the player's post-reconciliation squad.
func (r *SquadRepository) ReconcileSquads(ctx context.Context, apply bool) (*ReconcileReport, error) {
	var squads []Squad
	if cur, err := r.squads.Find(ctx, bson.M{}); err != nil {
		return nil, err
	} else if err := cur.All(ctx, &squads); err != nil {
		return nil, err
	}

	// player -> the squads they are currently in.
	byXUID := map[string][]squadMembership{}
	for _, sq := range squads {
		size := len(sq.Members)
		for _, m := range sq.Members {
			if m.XUID == "" {
				continue
			}
			byXUID[m.XUID] = append(byXUID[m.XUID], squadMembership{TeamID: sq.TeamID, Leader: m.Leader, Size: size})
		}
	}

	// current profile.teamId pointers.
	profileTeam := map[string]string{}
	var profs []CombasProfile
	if cur, err := r.profiles.Find(ctx, bson.M{}); err != nil {
		return nil, err
	} else if err := cur.All(ctx, &profs); err != nil {
		return nil, err
	}
	for _, p := range profs {
		profileTeam[p.XUID] = p.TeamID
	}

	report := &ReconcileReport{Applied: apply, SquadsScanned: len(squads)}

	// expected[xuid] = the squad the player should end up in (for the profile-pointer repair below).
	expected := map[string]string{}

	// 1. dedup multi-squad players.
	for xuid, ms := range byXUID {
		if len(ms) <= 1 {
			expected[xuid] = ms[0].TeamID
			continue
		}
		report.MultiSquadPlayers++
		plan := planPlayer(xuid, ms, profileTeam[xuid])
		expected[xuid] = plan.Keep
		for _, d := range plan.Dispositions {
			switch d.Kind {
			case "pull":
				report.Pulls = append(report.Pulls, fmt.Sprintf("%s pulled from %s", xuid, d.TeamID))
				if apply {
					if _, err := r.squads.UpdateOne(ctx, bson.M{"teamId": d.TeamID}, bson.M{"$pull": bson.M{"members": bson.M{"xuid": xuid}}, "$inc": bumpSeq}); err != nil {
						return nil, err
					}
				}
			case "disband":
				report.Disbands = append(report.Disbands, fmt.Sprintf("%s disbanded (orphaned solo squad led by %s)", d.TeamID, xuid))
				if apply {
					if _, err := r.squads.DeleteOne(ctx, bson.M{"teamId": d.TeamID}); err != nil {
						return nil, err
					}
				}
			case "flag":
				report.Flags = append(report.Flags, fmt.Sprintf("%s leads %s (has followers) but home = %s — MANUAL review (reassign leader or make this home)", xuid, d.TeamID, plan.Keep))
			}
		}
	}

	// 2. repair profile.teamId to match actual membership. A profile pointing at a squad the player is no
	// longer in (or was never in) is the other half of the inconsistency; a profile for a player now in no
	// squad is cleared. expected has an entry for every player currently in a squad; anyone else -> "".
	for xuid, cur := range profileTeam {
		want := expected[xuid] // "" if the player is in no squad
		if cur == want {
			continue
		}
		report.ProfileFixes = append(report.ProfileFixes, fmt.Sprintf("%s profile.teamId %q -> %q", xuid, cur, want))
		if apply {
			if _, err := r.profiles.UpdateOne(ctx, bson.M{"xuid": xuid}, bson.M{"$set": bson.M{"teamId": want}}); err != nil {
				return nil, err
			}
		}
	}

	return report, nil
}

// ContributionBackfillReport summarises a BackfillMemberContributions run (or its dry-run plan).
type ContributionBackfillReport struct {
	Applied bool
	Scanned int
	Changes []string
}

// BackfillMemberContributions seeds the per-member renown ledger (SquadMemberRecord.RenownContribution) for
// squads that accrued renown under the OLD flat-share model, before that field existed. For each squad it
// distributes the UNATTRIBUTED renown -- the squad's Renown.Total minus what its members' ledgers already
// sum to -- equally across the current roster, so a withdraw debits a real share instead of 0.
//
// Idempotent and composable: once the ledger sums to the total the gap is 0 and re-running is a no-op; and
// because it reuses the same equal-split credit a battle uses (CreditMemberContributions), it fills only the
// REMAINING gap on top of any real contributions already recorded. The past cannot be attributed accurately
// (old battle reports never recorded who fought), so an equal split is the honest approximation -- it matches
// the flat Renown/N the old debit assumed, while new battles credit actual participants going forward. Skips
// squads already consistent, with no renown, or whose gap is smaller than the roster (negligible remainder
// left unattributed, favouring the squad -- the same flooring the credit/debit use).
func (r *SquadRepository) BackfillMemberContributions(ctx context.Context, apply bool) (ContributionBackfillReport, error) {
	report := ContributionBackfillReport{Applied: apply}

	var squads []Squad
	cur, err := r.squads.Find(ctx, bson.M{})
	if err != nil {
		return report, err
	}
	if err := cur.All(ctx, &squads); err != nil {
		return report, err
	}

	// One read of the whole stats collection (matches BackfillSquadGrades): this scans every squad, so a
	// per-squad lookup would be N round-trips for nothing.
	var stats []SquadStats
	scur, err := r.stats.Find(ctx, bson.M{})
	if err != nil {
		return report, err
	}
	if err := scur.All(ctx, &stats); err != nil {
		return report, err
	}
	renownByTeam := make(map[string]int32, len(stats))
	for _, s := range stats {
		renownByTeam[s.TeamID] = s.Renown.Total
	}

	for _, sq := range squads {
		report.Scanned++
		n := len(sq.Members)
		if n == 0 {
			continue
		}
		total := renownByTeam[sq.TeamID] // absent stats doc -> 0 -> no gap, skipped below
		var attributed int32
		userIDs := make([]string, 0, n)
		for _, m := range sq.Members {
			attributed += m.RenownContribution
			userIDs = append(userIDs, m.UserID)
		}
		gap := total - attributed
		if gap <= 0 {
			continue // already consistent (or over-attributed), or no renown
		}
		share := gap / int32(n)
		if share <= 0 {
			continue // gap smaller than the roster; negligible remainder stays unattributed
		}
		report.Changes = append(report.Changes,
			fmt.Sprintf("%s %q: renown %d, attributed %d -> distribute gap %d across %d members (+%d each)",
				sq.TeamID, sq.Name, total, attributed, gap, n, share))

		if !apply {
			continue
		}
		// The backfill IS an equal-split credit of the historical gap, so reuse the battle-time path.
		if err := r.CreditMemberContributions(ctx, sq.TeamID, userIDs, gap); err != nil {
			return report, err
		}
	}
	return report, nil
}

// GrantReport summarises a GrantSquadRenown run (or its dry-run plan).
type GrantReport struct {
	Applied   bool
	TeamID    string
	Found     bool
	Members   int
	Renown    int32 // total granted
	PerMember int32 // renown / members (floored)
	Remainder int32 // renown % members (left unattributed, like the credit/debit flooring)
}

// GrantSquadRenown adds renown to ONE squad and distributes it evenly across its current members' ledgers --
// an admin compensation lever for squads unfairly drained by the old flat-share withdraw model. It raises the
// squad's Renown (running total + current-season bucket, so its ranking recovers) AND each member's
// RenownContribution (renown/N, floored; the remainder stays unattributed), keeping the ledger consistent so
// future withdraws debit correctly. The stored grade is refreshed to match, same as the battle path.
//
// Found=false (no error) for an unknown team id; errors on a non-positive amount or an empty roster. Uses the
// process's currentSeason, so the reconcile tool loads the DB season first (ApplySeasonNumber) to bucket it
// correctly.
func (r *SquadRepository) GrantSquadRenown(ctx context.Context, teamID string, renown int32, apply bool) (GrantReport, error) {
	rep := GrantReport{Applied: apply, TeamID: teamID, Renown: renown}
	if renown <= 0 {
		return rep, fmt.Errorf("renown to grant must be positive, got %d", renown)
	}
	sq, err := r.SquadByTeamID(ctx, teamID)
	if err != nil {
		return rep, err
	}
	if sq == nil {
		return rep, nil // Found=false
	}
	rep.Found = true
	rep.Members = len(sq.Members)
	if rep.Members == 0 {
		return rep, fmt.Errorf("squad %s has no members to distribute renown to", teamID)
	}
	n := int32(rep.Members)
	rep.PerMember = renown / n
	rep.Remainder = renown % n

	if !apply {
		return rep, nil
	}

	// Raise the squad's renown standing (running total + current-season bucket).
	if _, err := r.stats.UpdateOne(ctx, bson.M{"teamId": teamID},
		bson.M{"$inc": bson.M{"renown.total": renown, "renown.bySeason." + currentSeason: renown}},
		options.UpdateOne().SetUpsert(true)); err != nil {
		return rep, err
	}
	// Distribute to the per-member ledger (renown/N each) via the same equal-split credit a battle uses.
	userIDs := make([]string, 0, rep.Members)
	for _, m := range sq.Members {
		userIDs = append(userIDs, m.UserID)
	}
	if err := r.CreditMemberContributions(ctx, teamID, userIDs, renown); err != nil {
		return rep, err
	}
	// Keep the stored grade in step with the new renown, exactly as the battle path does after a credit.
	if _, err := r.RefreshSquadGrade(ctx, teamID); err != nil {
		logging.Warn.Printf("grant: grade refresh for %s failed: %v", teamID, err)
	}
	return rep, nil
}
