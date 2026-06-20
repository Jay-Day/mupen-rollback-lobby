package protocol

import "encoding/json"

// Envelope is the outer JSON shape for every WS message.
// id is optional, populated on requests so responses can be correlated.
type Envelope struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Client → Server message types
const (
	TypeHello              = "HELLO"
	TypeHeartbeat          = "HEARTBEAT"
	TypeChatSend           = "CHAT_SEND"
	TypeChatHistory        = "CHAT_HISTORY"
	TypeRoomCreate         = "ROOM_CREATE"
	TypeRoomJoin           = "ROOM_JOIN"
	TypeRoomLeave          = "ROOM_LEAVE"
	TypeRoomKick           = "ROOM_KICK"
	TypeRoomStart          = "ROOM_START"
	TypeRoomUpdateSettings = "ROOM_UPDATE_SETTINGS"
	TypePingProbeRequest   = "PING_PROBE_REQUEST"
	TypeMatchConnected     = "MATCH_CONNECTED"
	TypeMatchPunchFailed   = "MATCH_PUNCH_FAILED"
	TypeMatchFinished      = "MATCH_FINISHED"
	TypeQuickMatchJoin     = "QUICK_MATCH_JOIN"
	TypeQuickMatchCancel   = "QUICK_MATCH_CANCEL"

	// Spectating: one player streams the match's .krec up via BROADCAST_*; other
	// users pull it back down via SPECTATE_*. krec bytes ride as base64 strings
	// inside the normal JSON envelopes (the stream is ~1 KB/s, so the encoding
	// overhead is negligible).
	TypeBroadcastBegin = "BROADCAST_BEGIN" // broadcaster: start streaming this match
	TypeBroadcastData  = "BROADCAST_DATA"  // broadcaster: a chunk of krec bytes (base64)
	TypeBroadcastEnd   = "BROADCAST_END"   // broadcaster: stream finished
	TypeSpectateStart  = "SPECTATE_START"  // spectator: begin watching a match
	TypeSpectateStop   = "SPECTATE_STOP"   // spectator: stop watching
)

// Server → Client message types
const (
	TypeHelloOK          = "HELLO_OK"
	TypeHelloFail        = "HELLO_FAIL"
	TypeHeartbeatAck     = "HEARTBEAT_ACK"
	TypeChatMsg          = "CHAT_MSG"
	TypeChatHistoryReply = "CHAT_HISTORY_REPLY"
	TypePresenceFull     = "PRESENCE_FULL"
	TypePresenceDelta    = "PRESENCE_DELTA"
	TypeRoomCreated      = "ROOM_CREATED"
	TypeRoomList         = "ROOM_LIST"
	TypeRoomState        = "ROOM_STATE"
	TypeRoomJoinOK       = "ROOM_JOIN_OK"
	TypeRoomJoinFail     = "ROOM_JOIN_FAIL"
	TypeRoomCreateFail   = "ROOM_CREATE_FAIL"
	TypeRoomLeft         = "ROOM_LEFT"
	TypePingProbeReply   = "PING_PROBE_REPLY"
	TypeMatchBegin       = "MATCH_BEGIN"
	TypeMatchPeerLeft    = "MATCH_PEER_LEFT"
	TypeQuickMatchStatus = "QUICK_MATCH_STATUS"

	// Spectating (server → spectator).
	TypeSpectateBegin = "SPECTATE_BEGIN" // ack: stream is starting; history follows
	TypeSpectateData  = "SPECTATE_DATA"  // a chunk of krec bytes (base64)
	TypeSpectateEnd   = "SPECTATE_END"   // stream ended (match over / broadcaster left)
	TypeSpectateFail  = "SPECTATE_FAIL"  // request rejected (not broadcasting, ended)
)

// Failure reason codes
const (
	ReasonUsernameTaken   = "username_taken"
	ReasonVersionMismatch = "version_mismatch"
	ReasonServerFull      = "server_full"
	ReasonInvalidPayload  = "invalid_payload"
	ReasonInvalidHello    = "invalid_hello"
	ReasonRoomFull        = "full"
	ReasonWrongPassword   = "wrong_password"
	ReasonRomMismatch     = "rom_mismatch"
	ReasonAlreadyStarted  = "already_started"
	ReasonNotAuthorized   = "not_authorized"
	ReasonRoomNotFound    = "room_not_found"
	ReasonAlreadyInRoom   = "already_in_room"
	ReasonKickedRecently  = "kicked_recently"
)

// -- Request payloads --

type HelloRequest struct {
	Username      string   `json:"username"`
	ClientVersion string   `json:"clientVersion"`
	RomHashes     []string `json:"romHashes"`
	LocalIP       string   `json:"localIp,omitempty"`
}

type HeartbeatRequest struct {
	PingToServer uint16 `json:"pingToServer,omitempty"`
}

type ChatSendRequest struct {
	Channel string `json:"channel"`
	Message string `json:"message"`
}

type ChatHistoryRequest struct {
	Channel string `json:"channel"`
}

type RoomCreateRequest struct {
	Name       string  `json:"name"`
	Rom        RomInfo `json:"rom"`
	MaxPlayers int     `json:"maxPlayers"`
	Delay      int     `json:"delay"`
	Prediction int     `json:"prediction"`
	Pacing     int     `json:"pacing,omitempty"`
	Visibility string  `json:"visibility,omitempty"`
	Password   string  `json:"password,omitempty"`
}

type RomInfo struct {
	Name   string `json:"name"`
	MD5    string `json:"md5"`
	Region string `json:"region"`
}

type RoomJoinRequest struct {
	RoomID   uint64 `json:"roomId"`
	Password string `json:"password,omitempty"`
}

type RoomKickRequest struct {
	UserID uint64 `json:"userId"`
}

// RoomUpdateSettingsRequest changes a waiting room's rollback parameters.
// Host-only; the server clamps to [0,9] and rebroadcasts ROOM_STATE so every
// seated client (and the match itself) picks the new values up. The *Auto flags
// carry whether the host left each setting on "Auto" so non-hosts can render the
// same "Auto (N f)" label the host sees instead of a bare number.
type RoomUpdateSettingsRequest struct {
	Delay          int  `json:"delay"`
	Prediction     int  `json:"prediction"`
	Pacing         int  `json:"pacing,omitempty"`
	DelayAuto      bool `json:"delayAuto"`
	PredictionAuto bool `json:"predictionAuto"`
}

type PingProbeRequestPayload struct {
	TargetUserID uint64 `json:"targetUserId"`
}

type MatchPeerEventRequest struct {
	MatchID    uint64 `json:"matchId"`
	PeerUserID uint64 `json:"peerUserId,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// QuickMatchJoinRequest scopes a quick-match search to a specific ROM. The
// matchmaker pairs only searchers whose Rom.MD5 agree; Rom.Name is carried so
// the auto-created room can be resolved by name on both clients.
type QuickMatchJoinRequest struct {
	Rom RomInfo `json:"rom"`
}

// -- Response payloads --

type HelloOKResponse struct {
	UserID     uint64 `json:"userId"`
	ObservedIP string `json:"observedIp"`
	UDPAnchor  string `json:"udpAnchor"`
	Region     string `json:"region"`
}

type HelloFailResponse struct {
	Reason string `json:"reason"`
}

type HeartbeatAckResponse struct {
	ServerTime int64 `json:"serverTime"`
}

type ChatMsgPayload struct {
	Channel      string `json:"channel"`
	FromUserID   uint64 `json:"fromUserId"`
	FromUsername string `json:"fromUsername"`
	Message      string `json:"message"`
	ServerTime   int64  `json:"serverTime"`
}

type ChatHistoryReply struct {
	Channel  string           `json:"channel"`
	Messages []ChatMsgPayload `json:"messages"`
}

type PresenceUser struct {
	ID              uint64 `json:"id"`
	Username        string `json:"username"`
	State           string `json:"state"`
	Region          string `json:"region"`
	PingToServer    uint16 `json:"pingToServer"`
	CurrentRoomID   uint64 `json:"currentRoomId,omitempty"`
	CurrentRoomName string `json:"currentRoomName,omitempty"`
	SearchingRom    string `json:"searchingRom,omitempty"` // ROM name while state == searching
}

type PresenceFullPayload struct {
	Users []PresenceUser `json:"users"`
}

type PresenceDeltaPayload struct {
	Added   []PresenceUser `json:"added,omitempty"`
	Removed []uint64       `json:"removed,omitempty"`
	Updated []PresenceUser `json:"updated,omitempty"`
}

type RoomCreatedResponse struct {
	RoomID uint64 `json:"roomId"`
}

type RoomSummary struct {
	ID           uint64   `json:"id"`
	Name         string   `json:"name"`
	HostID       uint64   `json:"hostId"`
	HostName     string   `json:"hostName"`
	Rom          RomInfo  `json:"rom"`
	Players      int      `json:"players"`
	MaxPlayers   int      `json:"maxPlayers"`
	State        string   `json:"state"`
	HasPasswd    bool     `json:"hasPassword"`
	PlayerNames  []string `json:"playerNames,omitempty"`  // seated players, for the Ongoing Matches list
	StartedAt    int64    `json:"startedAt,omitempty"`    // match start, unix ms (0 until in-game)
	Broadcasting bool     `json:"broadcasting,omitempty"` // a player is streaming this match for spectators
	MatchID      uint64   `json:"matchId,omitempty"`      // current match id (set while in-game), for SPECTATE_START
}

type RoomListPayload struct {
	Rooms []RoomSummary `json:"rooms"`
}

type RoomSeatPayload struct {
	UserID   uint64 `json:"userId"`
	Username string `json:"username"`
	Slot     int    `json:"slot"`
}

type RoomStatePayload struct {
	ID         uint64  `json:"id"`
	Name       string  `json:"name"`
	HostID     uint64  `json:"hostId"`
	Rom        RomInfo `json:"rom"`
	MaxPlayers int     `json:"maxPlayers"`
	Delay      int     `json:"delay"`
	Prediction int     `json:"prediction"`
	// Pacing time-sync model (0 = aggressive, 1 = smooth); host-set, applies to
	// every seat. Discrete choice, so no Auto flag.
	Pacing int `json:"pacing"`
	// Whether each value is host-Auto-driven, so non-hosts can mirror the
	// host's "Auto (N f)" / "Default" labels rather than showing a bare number.
	DelayAuto      bool              `json:"delayAuto"`
	PredictionAuto bool              `json:"predictionAuto"`
	Players        []RoomSeatPayload `json:"players"`
	State          string            `json:"state"`
}

type RoomJoinFailPayload struct {
	Reason string `json:"reason"`
}

// RoomLeftPayload tells the client why it left a room: "left" (self),
// "kicked" (removed by host), or "host_left" (host dissolved the room).
type RoomLeftPayload struct {
	Reason string `json:"reason,omitempty"`
}

type PingProbeReplyPayload struct {
	TargetUserID   uint64 `json:"targetUserId"`
	TargetEndpoint string `json:"targetEndpoint"`
}

type MatchPeer struct {
	UserID     uint64 `json:"userId"`
	Username   string `json:"username"`
	PublicIP   string `json:"publicIp"`
	PublicPort uint16 `json:"publicPort"`
	LocalIP    string `json:"localIp,omitempty"`
	Slot       int    `json:"slot"`
}

type MatchBeginPayload struct {
	MatchID uint64      `json:"matchId"`
	Peers   []MatchPeer `json:"peers"`
}

type MatchPeerLeftPayload struct {
	MatchID  uint64 `json:"matchId"`
	UserID   uint64 `json:"userId"`
	Username string `json:"username"`
	Reason   string `json:"reason"`
	// Slot is the dropped player's 1-indexed controller seat. Clients can use
	// it to tell the user which port goes idle for the rest of the match.
	// 0 if the user wasn't seated (shouldn't happen in practice).
	Slot int `json:"slot,omitempty"`
}

type QuickMatchStatusPayload struct {
	Searching bool `json:"searching"`
	QueueSize int  `json:"queueSize"`
}

// -- Broadcast / spectate payloads --
// krec bytes travel as base64 in the Data field. The same shape is reused for
// the broadcaster's uplink (BROADCAST_DATA) and the spectator's downlink
// (SPECTATE_DATA) so the server can forward a chunk verbatim.

type BroadcastBeginRequest struct {
	MatchID uint64 `json:"matchId"`
}

type BroadcastDataRequest struct {
	MatchID uint64 `json:"matchId"`
	Data    string `json:"data"`  // base64 krec chunk
	Frame   int    `json:"frame"` // broadcaster's live krec frame after this chunk
}

type BroadcastEndRequest struct {
	MatchID uint64 `json:"matchId"`
}

type SpectateStartRequest struct {
	MatchID uint64 `json:"matchId"`
}

type SpectateStopRequest struct {
	MatchID uint64 `json:"matchId"`
}

type SpectateBeginPayload struct {
	MatchID uint64 `json:"matchId"`
}

type SpectateDataPayload struct {
	MatchID uint64 `json:"matchId"`
	Data    string `json:"data"`  // base64 krec chunk
	Frame   int    `json:"frame"` // broadcaster's live krec frame (fast-forward target)
}

type SpectateEndPayload struct {
	MatchID uint64 `json:"matchId"`
	Reason  string `json:"reason,omitempty"`
}

type SpectateFailPayload struct {
	MatchID uint64 `json:"matchId"`
	Reason  string `json:"reason"`
}
