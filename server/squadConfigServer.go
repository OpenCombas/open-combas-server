package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Squad config / settings upload (port 1205 = 1020+msgCode 185; retail 1245). Builder sub_823BEFA0 sends
// a fixed 60-byte body: gamertag(16) + teamid(18) + a 26-byte settings blob. Parser sub_823BD9D8 reads
// body[0]: '1' "Team Config Complete", '2' "User Number Error Or Other User Editing", '3' "Target Team
// Not Exist".
//
// Blob layout (offsets relative to the 60-byte body), recovered by differential analysis of a create vs
// a change-settings capture plus the packer sub_823C0790:
//   [34]    reserved
//   [35]    section flags: bit0 = main settings present, bit1 = colours/passcode present (the client
//           only sends the section it changed)
//   [36]    reserved config byte
//   [37..39] team colours      } colours/passcode section (present when flags&2)
//   [40..48] passcode (string) }
//   [49]    reserved (colours/passcode section)
//   [50] stance, [51] activity, [52] language, [53] regions, [54] role bitmask (main section, flags&1)
//   [56..59] reserved int
//
// We section-merge into the squad doc so editing one section never wipes the other, and answer '1' if
// the team exists, '3' otherwise. With no repository (Mongo off) this degrades to a plain '1' ack.
// See project_combas_server_protocol memory.

const squadConfigBodySize = 60

// parseSquadConfig extracts the team id, section flags and settings from a 60-byte config body.
func parseSquadConfig(packet []byte) (teamID string, flags byte, settings SquadSettings, ok bool) {
	if len(packet) < constants.MinHelloMessageSize+squadConfigBodySize {
		return "", 0, SquadSettings{}, false
	}
	body := packet[constants.MinHelloMessageSize:]

	teamRaw := body[16:34] // "TM" + 16 digits
	if i := bytes.IndexByte(teamRaw, 0); i >= 0 {
		teamRaw = teamRaw[:i]
	}
	teamID = string(teamRaw)

	blob := body[34:60] // 26-byte settings blob
	flags = blob[1]     // body[35]

	// main section (flags&1): blob[16..20] == body[50..54]
	settings.Stance = int32(blob[16])
	settings.Activity = int32(blob[17])
	settings.Language = int32(blob[18])
	settings.Regions = int32(blob[19])
	settings.RoleFlags = int32(blob[20])

	// colours/passcode section (flags&2): blob[3..5] colours, blob[6..14] passcode
	settings.Colors = append([]byte(nil), blob[3:6]...)
	pc := blob[6:15]
	if i := bytes.IndexByte(pc, 0); i >= 0 {
		pc = pc[:i]
	}
	settings.Passcode = string(pc)

	return teamID, flags, settings, true
}

type squadConfigServer struct {
	*messageServer
	repo *SquadRepository // nil when Mongo is disabled -> plain '1' ack
}

func (s *squadConfigServer) buildConfig(hi UserHelloMessage, packet []byte) SquadAckState {
	if s.repo == nil {
		return squadAckState(hi.Xuid, hi.Order, '1')
	}

	teamID, flags, settings, ok := parseSquadConfig(packet)
	if !ok || teamID == "" {
		logging.Warn.Printf("[%s] could not parse config upload, acking", s.serverConfig.Label)
		return squadAckState(hi.Xuid, hi.Order, '1')
	}

	readCtx, cancel := context.WithTimeout(s.ctx, worldReadTimeout)
	defer cancel()
	exists, err := s.repo.UpdateSquadSettings(readCtx, teamID, flags, settings)
	if err != nil {
		logging.Warn.Printf("[%s] settings persist failed, acking: %v", s.serverConfig.Label, err)
		return squadAckState(hi.Xuid, hi.Order, '1')
	}
	if !exists {
		return squadAckState(hi.Xuid, hi.Order, '3') // Target Team Not Exist
	}
	logging.Info.Printf("[%s] stored settings for %s (flags=%#x)", s.serverConfig.Label, teamID, flags)
	return squadAckState(hi.Xuid, hi.Order, '1') // Team Config Complete
}

func NewSquadConfigServer(listenAddress net.IP, serverConfig config.ServerConfig, bufferSize int, loggingConfig *config.LoggingConfig, ctx context.Context, wg *sync.WaitGroup, promConfig config.PrometheusConfig, reg prometheus.Registerer, repo *SquadRepository) *squadConfigServer {
	s := &squadConfigServer{repo: repo}

	s.messageServer = &messageServer{
		listenAddress: listenAddress,
		serverConfig:  &serverConfig,
		bufferSize:    bufferSize,
		loggingConfig: loggingConfig,
		ctx:           ctx,
		wg:            wg,
		promConfig:    promConfig,
		reg:           reg,

		validatePacket: func(packet []byte, clientAddr *net.UDPAddr) error {
			return validateWorldPacket(packet, clientAddr, serverConfig.Label)
		},
		buildResponse: func(readBuffer *[]byte) (*[]byte, error) {
			hi := s.parseHelloMessage(readBuffer)
			resp := s.buildConfig(hi, *readBuffer)
			buf := make([]byte, constants.SquadAckResponseSize)
			if _, err := binary.Encode(buf, binary.LittleEndian, resp); err != nil {
				return nil, err
			}
			return &buf, nil
		},
	}

	return s
}
