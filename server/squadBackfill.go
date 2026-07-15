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
// players collection's 1-day TTL -> fixes currently/recently-active players; the rest self-heal on reg/login.

// GamertagBackfillReport summarises the backfill (or the dry-run plan).
type GamertagBackfillReport struct {
	Applied        bool
	PlayersScanned int      // webservices login records that carry a gamertag
	ProfileFixes   []string // "<xuid>: <old> -> <truth>"
	RosterFixes    []string // "<xuid> in <teamId>: <old> -> <truth>"
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
					bson.M{"$set": bson.M{"members.$.gamertag": gt}},
				); err != nil {
					return nil, err
				}
			}
		}
	}

	return report, nil
}
