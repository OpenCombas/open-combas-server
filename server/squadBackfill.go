package server

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Retroactive gamertag backfill. The 182 join builder stamps the HOST's own gamertag onto joiners (it
// sources the gamertag from a host join-manager field instead of the joiner's handshake data — traced in
// IDA, sub_823BF1A0), so combas profiles/rosters carry wrong gamertags. RefreshGamertag self-heals these as
// each player next registers/logs in; this backfill fixes them immediately from an AUTHORITATIVE source:
// the Xenia-WebServices `players` collection, which stores xuid->gamertag from each console's own login
// (same shared DB). Both store the xuid as 16-char UPPERCASE hex, so the join is direct. Bounded by the
// players collection's 1-day TTL -> fixes currently/recently-active players. Rows the TTL can't reach are
// repaired by a second, heuristic pass keyed on the in-roster duplicate-tag signature (clobberedTagFixes);
// anything neither pass can prove self-heals on the player's next reg/login via RefreshGamertag.

// GamertagBackfillReport summarises the backfill (or the dry-run plan).
type GamertagBackfillReport struct {
	Applied        bool
	PlayersScanned int      // webservices login records that carry a gamertag
	ProfileFixes   []string // "<xuid>: <old> -> <truth>"
	RosterFixes    []string // "<xuid> in <teamId>: <old> -> <truth>"
	// Heuristic fixes from the in-roster duplicate-tag signature (see clobberedTagFixes), used only for
	// xuids the authoritative players collection doesn't cover.
	ClobberProfileFixes []string // "<xuid>: <old> -> <name>"
	ClobberRosterFixes  []string // "<xuid> in <teamId>: <old> -> <name>"
}

// clobberFix is one heuristic gamertag fix derived from the duplicate-tag signature within a roster.
type clobberFix struct {
	XUID, TeamID, Old, New string
}

// clobberedTagFixes detects the 182 host-stamp signature inside one roster: a gamertag shared by more
// than one member is impossible legitimately (Live tags are unique and membership is XUID-keyed), so a
// duplicated tag belongs to at most ONE of those rows. Rows carrying the shared tag alongside a
// DIFFERENT non-empty in-squad name take that name as the best-available gamertag (the name field is
// per-join handshake data, correctly sourced -- unlike the tag). Skipped: owner-looking rows (empty
// name, or name == tag: the tag may genuinely be theirs) and xuids covered by the authoritative pass.
// Mismatches WITHOUT an in-roster duplicate are left alone -- unprovable, they self-heal on login.
func clobberedTagFixes(sq Squad, truth map[string]string) []clobberFix {
	count := map[string]int{}
	for _, m := range sq.Members {
		if m.Gamertag != "" {
			count[m.Gamertag]++
		}
	}
	var fixes []clobberFix
	for _, m := range sq.Members {
		if m.Gamertag == "" || count[m.Gamertag] < 2 {
			continue
		}
		if _, hasTruth := truth[m.XUID]; hasTruth {
			continue // the authoritative pass owns this xuid (fixed there, or already correct)
		}
		if m.Name == "" || m.Name == m.Gamertag {
			continue // owner-looking row
		}
		fixes = append(fixes, clobberFix{XUID: m.XUID, TeamID: sq.TeamID, Old: m.Gamertag, New: m.Name})
	}
	return fixes
}

// BackfillGamertags corrects combas profiles + squad rosters from the webservices `players` collection.
// apply=false is a dry run (reports the plan, writes nothing).
func (r *SquadRepository) BackfillGamertags(ctx context.Context, players *mongo.Collection, apply bool) (*GamertagBackfillReport, error) {
	// Authoritative xuid -> gamertag from webservices logins (skip blank gamertags).
	type playerRec struct {
		XUID     string `bson:"xuid"`
		Gamertag string `bson:"gamertag"`
	}
	var recs []playerRec
	if cur, err := players.Find(ctx, bson.M{}); err != nil {
		return nil, err
	} else if err := cur.All(ctx, &recs); err != nil {
		return nil, err
	}
	truth := make(map[string]string, len(recs))
	for _, p := range recs {
		if p.Gamertag != "" {
			truth[p.XUID] = p.Gamertag
		}
	}

	report := &GamertagBackfillReport{Applied: apply, PlayersScanned: len(truth)}

	// Persistent profiles.
	var profs []CombasProfile
	if cur, err := r.profiles.Find(ctx, bson.M{}); err != nil {
		return nil, err
	} else if err := cur.All(ctx, &profs); err != nil {
		return nil, err
	}
	for _, pr := range profs {
		gt, ok := truth[pr.XUID]
		if !ok || gt == pr.Gamertag {
			continue
		}
		report.ProfileFixes = append(report.ProfileFixes, fmt.Sprintf("%s: %q -> %q", pr.XUID, pr.Gamertag, gt))
		if apply {
			if _, err := r.profiles.UpdateOne(ctx, bson.M{"xuid": pr.XUID}, bson.M{"$set": bson.M{"gamertag": gt}}); err != nil {
				return nil, err
			}
		}
	}

	// Denormalised roster entries (every squad the player is listed in).
	var squads []Squad
	if cur, err := r.squads.Find(ctx, bson.M{}); err != nil {
		return nil, err
	} else if err := cur.All(ctx, &squads); err != nil {
		return nil, err
	}
	for _, sq := range squads {
		for _, m := range sq.Members {
			gt, ok := truth[m.XUID]
			if !ok || gt == m.Gamertag {
				continue
			}
			report.RosterFixes = append(report.RosterFixes, fmt.Sprintf("%s in %s: %q -> %q", m.XUID, sq.TeamID, m.Gamertag, gt))
			if apply {
				if _, err := r.squads.UpdateOne(ctx,
					bson.M{"teamId": sq.TeamID, "members.xuid": m.XUID},
					bson.M{"$set": bson.M{"members.$.gamertag": gt}, "$inc": bumpSeq},
				); err != nil {
					return nil, err
				}
			}
		}
	}

	// Heuristic pass: repair clobbered rows the players collection can't reach (its TTL only covers
	// recently-active consoles) from the in-roster duplicate-tag signature. The roster row takes the
	// member's own join name; the profile is corrected only when it froze the SAME wrong tag (via the
	// EnsureProfile insert on join) -- a profile carrying some other value is left for login self-heal.
	profTag := make(map[string]string, len(profs))
	for _, pr := range profs {
		profTag[pr.XUID] = pr.Gamertag
	}
	for _, sq := range squads {
		for _, fix := range clobberedTagFixes(sq, truth) {
			report.ClobberRosterFixes = append(report.ClobberRosterFixes, fmt.Sprintf("%s in %s: %q -> %q", fix.XUID, fix.TeamID, fix.Old, fix.New))
			if apply {
				if _, err := r.squads.UpdateOne(ctx,
					bson.M{"teamId": fix.TeamID, "members.xuid": fix.XUID},
					bson.M{"$set": bson.M{"members.$.gamertag": fix.New}, "$inc": bumpSeq},
				); err != nil {
					return nil, err
				}
			}
			if profTag[fix.XUID] == fix.Old {
				report.ClobberProfileFixes = append(report.ClobberProfileFixes, fmt.Sprintf("%s: %q -> %q", fix.XUID, fix.Old, fix.New))
				if apply {
					if _, err := r.profiles.UpdateOne(ctx, bson.M{"xuid": fix.XUID}, bson.M{"$set": bson.M{"gamertag": fix.New}}); err != nil {
						return nil, err
					}
				}
			}
		}
	}

	return report, nil
}
