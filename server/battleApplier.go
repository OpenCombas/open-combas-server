package server

import (
	"context"
	"math"
	"time"

	"ChromehoundsStatusServer/logging"
)

// BattleApplier applies parsed battle results to the world + squad state under the winner-takes-all
// capture model (captures, locks, area cascades, HQ dissolution/revival, and the news events they
// raise). It holds only data-layer dependencies -- no network server -- so the live battle-report
// server, the war-simulation CLI, and tests all drive the SAME logic through it. This is the single
// source of truth for what a battle does to the world; nothing should re-implement it.
type BattleApplier struct {
	worldRepo      *WorldRepository
	squadRepo      *SquadRepository
	generateEvents bool
	cpuBattleScale float64 // PvE (CPU-opponent) occ+renown multiplier; always normalized to > 0
	label          string  // log prefix
}

// NewBattleApplier builds an applier. Either repo may be nil (that stage is then skipped); label is the
// log prefix (the owning server's config Label for the live path, the tool name for the CLI).
// cpuBattleScale multiplies the occupation + renown from a mission against a CPU squad; a value <= 0 is
// treated as 1.0 (no scaling) so an unset config never zeroes out rewards.
func NewBattleApplier(worldRepo *WorldRepository, squadRepo *SquadRepository, generateEvents bool, cpuBattleScale float64, label string) *BattleApplier {
	if cpuBattleScale <= 0 {
		cpuBattleScale = 1
	}
	return &BattleApplier{worldRepo: worldRepo, squadRepo: squadRepo, generateEvents: generateEvents, cpuBattleScale: cpuBattleScale, label: label}
}

// Apply processes one battle result exactly as the live battle-report server does: it applies the world
// mutation (unless the fought battlefield is locked, in which case the report is ignored) and credits
// squad renown. Safe to call outside the network server.
func (s *BattleApplier) Apply(ctx context.Context, res BattleResult) {
	// A mission against a CPU/AI opponent is PvE: scale the winner's occupation + renown before res fans
	// out, so the war map, the squad ledger, and the summary log all agree.
	res = scaleForCPU(res, s.cpuBattleScale)

	if s.worldRepo != nil && res.AreaID == WeaponBattleAreaID {
		// A weapon-destruction (boss) mission reports under the sentinel area 99 with the weapon as the
		// loser -- it moves no occupation, so handle it here instead of the normal battlefield path. Squad
		// renown for the attacking side is still credited below, exactly as for any other mission.
		s.applyWeaponReport(ctx, res)
	} else if s.worldRepo != nil {
		bf, err := s.worldRepo.BattlefieldByAreaMap(ctx, res.AreaID, res.MapID)
		switch {
		case err != nil:
			logging.Warn.Printf("[%s] battlefield %d/%d read failed: %v", s.label, res.AreaID, res.MapID, err)
		case bf == nil:
			logging.Warn.Printf("[%s] battle report for unknown battlefield %d/%d, acking without ingest", s.label, res.AreaID, res.MapID)
		case bf.Locked:
			// A locked battlefield accepts no missions, so a report for one is spurious: ignore it
			// entirely (no occupation, no counters, no squad renown) rather than let a stray packet
			// overwrite the capture that locked it.
			logging.Info.Printf("[%s] battlefield %d/%d locked (defeated %s, unlock@%d); ignoring report", s.label, res.AreaID, res.MapID, bf.DefeatedNation, bf.UnlockAtBattle)
			return
		default:
			s.applyBattle(ctx, res, *bf)
		}
	}
	if s.squadRepo != nil {
		if err := s.squadRepo.CreditBattle(ctx, res.WinnerTeam, res.LoserTeam, res.OccDelta, res.WinnerMerit, currentSeason); err != nil {
			logging.Warn.Printf("[%s] squad-stat credit failed: %v", s.label, err)
		} else {
			// Renown moved, so either side's grade may have crossed a rung -- persist it and raise the
			// matching up/down history event. BOTH teams are refreshed, not just the winner: renown can
			// decrease, so a loser's grade can fall. Best-effort like the credit itself; login derives the
			// grade live (see squadGrade.go), so a failure here costs the history event, not a wrong grade.
			for _, team := range []string{res.WinnerTeam, res.LoserTeam} {
				if !isRealTeam(team) {
					continue
				}
				if grade, err := s.squadRepo.RefreshSquadGrade(ctx, team); err != nil {
					logging.Warn.Printf("[%s] squad grade refresh failed for %s: %v", s.label, team, err)
				} else {
					logging.Info.Printf("[%s] squad %s grade now %d", s.label, team, grade)
				}
			}
		}
	}
	logging.Info.Printf("[%s] area %d/%d: winner %c/%s (+%d occ, +%d renown) vs %c/%s",
		s.label, res.AreaID, res.MapID, res.WinnerNation, res.WinnerTeam, res.OccDelta, res.WinnerMerit, res.LoserNation, res.LoserTeam)
}

// applyWeaponReport handles a weapon-destruction (boss) mission report (area 99). The weapon is the LOSER;
// the winner is the attacking nation. Each such report is one successful attack: it bumps the deployed
// weapon's hit counter, and when the count reaches the configured threshold the weapon is destroyed --
// its deployment is cleared (dropping the World deploy byte so it vanishes from the war map) and the
// "destroyed" news is filed. A report for a nation with no weapon deployed is ignored.
func (s *BattleApplier) applyWeaponReport(ctx context.Context, res BattleResult) {
	nation := res.LoserNation
	hits, threshold, found, err := s.worldRepo.RecordWeaponHit(ctx, nation)
	if err != nil {
		logging.Warn.Printf("[%s] weapon-hit record (%c) failed: %v", s.label, nation, err)
		return
	}
	if !found {
		logging.Info.Printf("[%s] weapon report (loser %c) but no weapon deployed for it; ignoring", s.label, nation)
		return
	}
	logging.Info.Printf("[%s] unidentified weapon (%c) took hit %d/%d", s.label, nation, hits, threshold)
	if threshold <= 0 || hits < threshold {
		return // no auto-destroy configured, or not enough hits yet
	}

	// Threshold reached -> destroy. Clearing the deployment drops the World deploy byte (weapon disappears).
	if _, err := s.worldRepo.ClearWeaponDeployed(ctx, nation); err != nil {
		logging.Warn.Printf("[%s] weapon clear (%c) failed: %v", s.label, nation, err)
	}
	if s.generateEvents {
		if _, err := s.worldRepo.RecordUnidentifiedWeaponEvent(ctx, nation, WeaponDestroyed, time.Now()); err != nil {
			logging.Warn.Printf("[%s] weapon destroyed event (%c) failed: %v", s.label, nation, err)
		}
	}
	logging.Info.Printf("[%s] EVENT: unidentified weapon (%c) DESTROYED after %d mission(s)", s.label, nation, hits)
}

// applyBattle applies one battle report to the world under the winner-takes-all capture model:
//   - both combatants' battle counters advance (the unlock clock for locked battlefields);
//   - a change of holder flips the fought battlefield 100% to the winner and locks out the loser;
//   - if that flips the area's owner, the whole area is surrendered (every battlefield flips + locks);
//   - reaching a nation's unlock threshold reopens the battlefields it had lost.
//
// A successful defence (the winner already held the battlefield) changes no occupation and issues no
// lock -- only the counters advance. Capture/region/conquest events fire only on a genuine change of
// hands.
func (s *BattleApplier) applyBattle(ctx context.Context, res BattleResult, bf Battlefield) {
	beforeLead := bfLead(bf)
	// A mission ADDS its OccDelta to the winner and removes it from the loser; the battlefield only changes
	// hands once that accumulated shift makes the attacker OVERTAKE the current holder. So capture is gated
	// on the post-shift lead crossing over -- not on merely winning a single mission here.
	newLead := leadAfterDelta(bf, res.WinnerNation, res.LoserNation, res.OccDelta)
	changedHands := beforeLead != 0 && newLead != 0 && newLead != beforeLead

	// Area owner before the battle -- needed to detect an area surrender / HQ fall and fire events.
	var beforeOwner byte
	if s.generateEvents {
		beforeOwner, _, _ = s.worldRepo.AreaAndBFLead(ctx, int32(res.AreaID), int32(res.MapID))
	}

	// Both nations fought a battle: advance their counters. This is the "10 other battles across all
	// areas" clock that eventually reopens battlefields each has lost.
	winnerCount, err := s.worldRepo.IncrementBattleCount(ctx, res.WinnerNation)
	if err != nil {
		logging.Warn.Printf("[%s] battle-count bump (winner %c) failed: %v", s.label, res.WinnerNation, err)
	}
	loserCount, err := s.worldRepo.IncrementBattleCount(ctx, res.LoserNation)
	if err != nil {
		logging.Warn.Printf("[%s] battle-count bump (loser %c) failed: %v", s.label, res.LoserNation, err)
	}

	// Record this report toward the area's fierce-battle share (PvP = two distinct real squads).
	if err := s.worldRepo.CreditAreaBattle(ctx, int32(res.AreaID), isPvP(res)); err != nil {
		logging.Warn.Printf("[%s] area-battle credit (area %d) failed: %v", s.label, res.AreaID, err)
	}

	if changedHands {
		// Crossover -> winner-takes-all: snap the battlefield to 100% for the winner and lock out the former
		// holder until it fights UnlockBattleThreshold more battles. The former holder is normally the loser.
		defeated := beforeLead
		defeatedCount := loserCount
		if defeated != res.LoserNation {
			defeatedCount, _ = s.worldRepo.NationBattleCount(ctx, defeated)
		}
		if err := s.worldRepo.CaptureBattlefield(ctx, int32(res.AreaID), int32(res.MapID), res.WinnerNation, defeated, defeatedCount+UnlockBattleThreshold); err != nil {
			logging.Warn.Printf("[%s] capture flip %d/%d failed: %v", s.label, res.AreaID, res.MapID, err)
		}
	} else if err := s.worldRepo.ApplyBattleOccupation(ctx, res.AreaID, res.MapID, res.WinnerNation, res.LoserNation, res.OccDelta); err != nil {
		// No capture yet -> just accumulate this mission's occupation toward a future crossover.
		logging.Warn.Printf("[%s] occupation shift %d/%d failed: %v", s.label, res.AreaID, res.MapID, err)
	}

	// This battle may have carried either nation past a lock it was waiting out.
	if err := s.worldRepo.UnlockExpiredBattlefields(ctx, res.WinnerNation, winnerCount); err != nil {
		logging.Warn.Printf("[%s] unlock sweep (%c) failed: %v", s.label, res.WinnerNation, err)
	}
	if err := s.worldRepo.UnlockExpiredBattlefields(ctx, res.LoserNation, loserCount); err != nil {
		logging.Warn.Printf("[%s] unlock sweep (%c) failed: %v", s.label, res.LoserNation, err)
	}

	if !s.generateEvents {
		return
	}

	// Credit the winning squad's ledger (backs the |s2= "top squad of the capturing nation"), for
	// defences as well as captures so the ranking reflects all of a nation's activity in the area.
	if err := s.worldRepo.CreditCapture(ctx, int32(res.AreaID), int32(res.MapID), res.WinnerTeam, string(res.WinnerNation), res.OccDelta); err != nil {
		logging.Warn.Printf("[%s] capture-ledger credit failed: %v", s.label, err)
	}
	if !changedHands {
		return // a defence flips nothing -> no capture/region/conquest events
	}

	afterOwner, afterLead, _ := s.worldRepo.AreaAndBFLead(ctx, int32(res.AreaID), int32(res.MapID))
	areaFlipped := beforeOwner != 0 && afterOwner != 0 && beforeOwner != afterOwner

	// HQ transitions take priority over ordinary region/battlefield capture stories.
	if areaFlipped && isHQ(res.AreaID) {
		hqDefault := hqDefaultNation(res.AreaID)
		switch {
		case beforeOwner == hqDefault:
			// The capital's OWN nation lost it -> dissolution: cascade its remaining areas to the captor
			// and drop it into revival lockout. (An HQ held by a non-default nation falling is NOT a
			// dissolution -- it drops through to the normal region-capture path below.)
			s.recordHQFall(ctx, res, hqDefault, afterOwner)
			return
		case afterOwner == hqDefault:
			// The capital's own nation retook it -> revival, but only if it was actually dissolved.
			if captor, _ := s.worldRepo.NationHQLostTo(ctx, hqDefault); captor != 0 {
				s.recordRevival(ctx, res, hqDefault)
				return
			}
		}
	}

	// Normal area capture: the loser surrenders the whole area (flip + lock), then fire events.
	if areaFlipped {
		surCount, _ := s.worldRepo.NationBattleCount(ctx, beforeOwner)
		if err := s.worldRepo.FlipAndLockArea(ctx, int32(res.AreaID), afterOwner, beforeOwner, surCount+UnlockBattleThreshold); err != nil {
			logging.Warn.Printf("[%s] area surrender flip (area %d) failed: %v", s.label, res.AreaID, err)
		}
		// The area flipped -> its fierce-battle share restarts.
		if err := s.worldRepo.ResetAreaBattleStats(ctx, int32(res.AreaID)); err != nil {
			logging.Warn.Printf("[%s] area-stats reset (area %d) failed: %v", s.label, res.AreaID, err)
		}
	}

	s.maybeRecordCaptures(ctx, res, beforeOwner, beforeLead, afterOwner, afterLead)
}

// recordHQFall handles a capital changing hands away from its default nation: the fallen capital and
// every other area the dissolved nation still controls flip 100% to the captor (unlocked), the
// dissolved nation enters revival lockout (HQ-only missions until it retakes its capital), and a
// dissolution story is filed. loser is the capital's default nation, captor the new owner.
func (s *BattleApplier) recordHQFall(ctx context.Context, res BattleResult, loser, captor byte) {
	now := time.Now().Unix()

	// The fallen capital flips fully to the captor but stays UNLOCKED -- the dissolved nation has to be
	// able to attack it to revive. Its fierce-battle share restarts (the area flipped).
	if err := s.worldRepo.FlipAreaUnlocked(ctx, int32(res.AreaID), captor); err != nil {
		logging.Warn.Printf("[%s] HQ flip (area %d) failed: %v", s.label, res.AreaID, err)
	}
	if err := s.worldRepo.ResetAreaBattleStats(ctx, int32(res.AreaID)); err != nil {
		logging.Warn.Printf("[%s] area-stats reset (area %d) failed: %v", s.label, res.AreaID, err)
	}
	// Cascade: every OTHER area the loser still holds surrenders to the captor. Current control only --
	// an area of the loser's home territory that some third nation already took is not affected.
	owned, err := s.worldRepo.AreasOwnedBy(ctx, loser)
	if err != nil {
		logging.Warn.Printf("[%s] HQ cascade lookup (%c) failed: %v", s.label, loser, err)
	}
	cascaded := 0
	for _, a := range owned {
		if a == int32(res.AreaID) {
			continue
		}
		if err := s.worldRepo.FlipAreaUnlocked(ctx, a, captor); err != nil {
			logging.Warn.Printf("[%s] HQ cascade flip (area %d) failed: %v", s.label, a, err)
			continue
		}
		_ = s.worldRepo.ResetAreaBattleStats(ctx, a) // cascaded area flipped -> restart its share
		cascaded++
	}
	if err := s.worldRepo.SetNationHQLost(ctx, loser, captor); err != nil {
		logging.Warn.Printf("[%s] set dissolved (%c) failed: %v", s.label, loser, err)
	}

	row, ok := dissolutionRow(loser, captor)
	if !ok {
		return
	}
	if err := s.worldRepo.RecordEvent(ctx, EventRecord{CreatedAt: now, TemplateID: row}); err != nil {
		logging.Warn.Printf("[%s] record dissolution event failed: %v", s.label, err)
		return
	}
	logging.Info.Printf("[%s] EVENT: capital of %c fell to %c (area %d), %d area(s) cascaded, %c in revival lockout -> dissolution row %d",
		s.label, loser, captor, res.AreaID, cascaded, loser, row)
}

// recordRevival handles a dissolved nation retaking its capital: the HQ area flips fully back to it
// (unlocked), the revival lockout lifts (it may attack beyond its HQ again), and a revival story is
// filed. Areas lost in the cascade are NOT restored -- they are recaptured through normal play.
func (s *BattleApplier) recordRevival(ctx context.Context, res BattleResult, nation byte) {
	now := time.Now().Unix()
	if err := s.worldRepo.FlipAreaUnlocked(ctx, int32(res.AreaID), nation); err != nil {
		logging.Warn.Printf("[%s] revival HQ flip (area %d) failed: %v", s.label, res.AreaID, err)
	}
	if err := s.worldRepo.ResetAreaBattleStats(ctx, int32(res.AreaID)); err != nil {
		logging.Warn.Printf("[%s] area-stats reset (area %d) failed: %v", s.label, res.AreaID, err)
	}
	if err := s.worldRepo.ClearNationHQLost(ctx, nation); err != nil {
		logging.Warn.Printf("[%s] clear dissolved (%c) failed: %v", s.label, nation, err)
	}
	row, ok := revivalRow(nation)
	if !ok {
		logging.Info.Printf("[%s] EVENT: %c revived (retook capital area %d); revival news row pending RE", s.label, nation, res.AreaID)
		return
	}
	if err := s.worldRepo.RecordEvent(ctx, EventRecord{CreatedAt: now, TemplateID: row}); err != nil {
		logging.Warn.Printf("[%s] record revival event failed: %v", s.label, err)
		return
	}
	logging.Info.Printf("[%s] EVENT: %c revived (retook capital area %d) -> revival row %d", s.label, nation, res.AreaID, row)
}

// isPvP reports whether a battle report is squad-vs-squad: both sides are real squad teams (TM ids) and
// distinct. A battle against an AI/CPU side (a non-TM id) is PvE. Drives the area fierce-battle share.
func isPvP(res BattleResult) bool {
	return isRealTeam(res.WinnerTeam) && isRealTeam(res.LoserTeam) && res.WinnerTeam != res.LoserTeam
}

// scaleForCPU returns res with its winner occupation + renown multiplied by scale when the report is a
// real squad's win over a CPU/AI squad (a non-"TM", non-empty loser) -- the only case that earns points
// worth tuning. PvP, a real squad losing to a CPU (winner not real), and scale==1 all pass through
// unchanged, so PvP rewards and defeats are never altered.
func scaleForCPU(res BattleResult, scale float64) BattleResult {
	if scale == 1 || !isRealTeam(res.WinnerTeam) || res.LoserTeam == "" || isRealTeam(res.LoserTeam) {
		return res
	}
	res.OccDelta = scalePoints(res.OccDelta, scale)
	res.WinnerMerit = scalePoints(res.WinnerMerit, scale)
	return res
}

// scalePoints multiplies a non-negative point value (occupation or renown) by factor, rounding to the
// nearest int32 and clamping at 0. A zero/negative input is returned unchanged (nothing to scale).
func scalePoints(v int32, factor float64) int32 {
	if v <= 0 {
		return v
	}
	scaled := math.Round(float64(v) * factor)
	if scaled < 0 {
		return 0
	}
	return int32(scaled)
}

// squadDisplayName resolves a wire team id (e.g. "TM0001000000000042", read from the battle report at
// +0x26) to the squad's human-readable name for the |s2= "most-involved squad" news slot. The capture
// ledger keys on the stable team id; we resolve to the name only at render time. Falls back to the raw id
// when the squad is unknown (e.g. an AI/placeholder team) or the squad repo is unavailable, so the slot is
// never blank/worse than before.
func (s *BattleApplier) squadDisplayName(ctx context.Context, teamID string) string {
	if teamID == "" || s.squadRepo == nil {
		return teamID
	}
	sq, err := s.squadRepo.SquadByTeamID(ctx, teamID)
	if err != nil {
		logging.Warn.Printf("[%s] squad-name lookup for %q failed: %v", s.label, teamID, err)
		return teamID
	}
	if sq == nil || sq.Name == "" {
		return teamID
	}
	return sq.Name
}

// maybeRecordCaptures emits a battlefield-capture and/or a (non-HQ) region-capture WORLD_NEWS event when
// this battle changed the lead nation of the fought battlefield / area.
//
// Slot2 = the top-contributing squad NAME (|s2= renders it as a literal string). Slot1 = the place-name
// TEXT ID (NOT the name string): the region (|A1=/|a1=) and battlefield (|B1) placeholders are id-lookups --
// the client reads slot1 as a MenuText id and resolves the name itself (region ids 5020-5044, battlefield
// ids 5100-5209, from WorldSituationInfoNews{Area,Field}Param.bin). Writing the raw name string here renders
// blank; the id must be sent. areaNameSlot/battlefieldNameSlot yield that id as ASCII decimal.
func (s *BattleApplier) maybeRecordCaptures(ctx context.Context, res BattleResult, beforeOwner, beforeLead, afterOwner, afterLead byte) {
	now := time.Now().Unix()
	// Battlefield captured: the fought battlefield's lead nation changed.
	if beforeLead != 0 && afterLead != 0 && beforeLead != afterLead {
		teamID, _ := s.worldRepo.TopSquadForBattlefield(ctx, int32(res.AreaID), int32(res.MapID), string(afterLead))
		squad := s.squadDisplayName(ctx, teamID)
		if row, err := RecordBattlefieldCaptureEvent(ctx, s.worldRepo, int32(res.AreaID), int32(res.MapID), beforeLead, afterLead, squad, now); err != nil {
			logging.Warn.Printf("[%s] record battlefield-capture event failed: %v", s.label, err)
		} else if row != 0 {
			logging.Info.Printf("[%s] EVENT: battlefield %q (%d/%d) captured %c->%c (squad %q) -> row %d", s.label, battlefieldName(int32(res.AreaID), int32(res.MapID)), res.AreaID, res.MapID, beforeLead, afterLead, squad, row)
		}
	}
	// Region captured: the AREA's owner changed (HQ dissolutions are filtered out by the caller).
	if beforeOwner != 0 && afterOwner != 0 && beforeOwner != afterOwner {
		teamID, _ := s.worldRepo.TopSquadForRegion(ctx, int32(res.AreaID), string(afterOwner))
		squad := s.squadDisplayName(ctx, teamID)
		if row, err := RecordRegionCaptureEvent(ctx, s.worldRepo, int32(res.AreaID), beforeOwner, afterOwner, squad, now); err != nil {
			logging.Warn.Printf("[%s] record region-capture event failed: %v", s.label, err)
		} else if row != 0 {
			logging.Info.Printf("[%s] EVENT: region %q (%d) captured %c->%c (squad %q) -> row %d", s.label, areaName(int32(res.AreaID)), res.AreaID, beforeOwner, afterOwner, squad, row)
		}
	}
}
