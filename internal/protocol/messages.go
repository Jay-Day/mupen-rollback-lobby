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
	TypeHello                    = "HELLO"
	TypeHeartbeat                = "HEARTBEAT"
	TypeChatSend                 = "CHAT_SEND"
	TypeChatHistory              = "CHAT_HISTORY"
	TypeRoomCreate               = "ROOM_CREATE"
	TypeRoomJoin                 = "ROOM_JOIN"
	TypeRoomLeave                = "ROOM_LEAVE"
	TypeRoomKick                 = "ROOM_KICK"
	TypeRoomStart                = "ROOM_START"
	TypeRoomUpdateSettings       = "ROOM_UPDATE_SETTINGS"
	TypeRoomUpdateDelay          = "ROOM_UPDATE_DELAY"
	TypeRoomUpdatePlayerSettings = "ROOM_UPDATE_PLAYER_SETTINGS"
	TypeRoomUpdatePrediction     = "ROOM_UPDATE_PREDICTION"
	TypeRoomUpdateLiveReplay     = "ROOM_UPDATE_LIVE_REPLAY"
	TypeRoomSwapSlots            = "ROOM_SWAP_SLOTS"
	TypePingProbeRequest         = "PING_PROBE_REQUEST"
	TypeMatchConnected           = "MATCH_CONNECTED"
	TypeMatchPunchFailed         = "MATCH_PUNCH_FAILED"
	TypeMatchFinished            = "MATCH_FINISHED"
	TypeQuickMatchJoin           = "QUICK_MATCH_JOIN"
	TypeQuickMatchCancel         = "QUICK_MATCH_CANCEL"
	// ICE signaling is relayed over the authenticated lobby WebSocket. The
	// server never sees gameplay datagrams and is not a TURN relay.
	TypeICESignal = "ICE_SIGNAL"

	// Spectating: one player streams the match's .krec up via BROADCAST_*; other
	// users pull it back down via SPECTATE_*. krec bytes ride as base64 strings
	// inside the normal JSON envelopes (the stream is ~1 KB/s, so the encoding
	// overhead is negligible).
	TypeBroadcastBegin = "BROADCAST_BEGIN" // broadcaster: start streaming this match
	TypeBroadcastData  = "BROADCAST_DATA"  // broadcaster: a chunk of krec bytes (base64)
	TypeBroadcastEnd   = "BROADCAST_END"   // broadcaster: stream finished
	// Periodic savestate "keyframe": the broadcaster uploads a compressed savestate
	// periodically so a late-joining spectator can restore near the live edge and
	// fast-forward only the short tail (bounded catch-up, independent of match length).
	// Sent in chunks to stay under the server's per-message read limit.
	TypeBroadcastKeyframe = "BROADCAST_KEYFRAME"
	TypeSpectateStart     = "SPECTATE_START" // spectator: begin watching a match
	TypeSpectateStop      = "SPECTATE_STOP"  // spectator: stop watching

	// Moderation (client → server).
	TypeAdminAuth = "ADMIN_AUTH" // claim moderator role with a password
	TypeModAction = "MOD_ACTION" // moderator: kick/mute/timeout/ban/unban/unmute/list
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
	// Unsolicited: tells the recipient that someone wants to measure ping with
	// them, so both sides punch at the same time. A lone PING_PROBE_REPLY only
	// arms the requester, whose UDP probe then dies at the target's NAT unless
	// the target happens to have an open mapping already. Pushed on room join
	// and alongside each PING_PROBE_REPLY.
	TypePingProbeIncoming = "PING_PROBE_INCOMING"
	TypeMatchBegin        = "MATCH_BEGIN"
	TypeMatchPeerLeft     = "MATCH_PEER_LEFT"
	TypeQuickMatchStatus  = "QUICK_MATCH_STATUS"

	// Spectating (server → spectator).
	TypeSpectateBegin = "SPECTATE_BEGIN" // ack: stream is starting; history follows
	TypeSpectateData  = "SPECTATE_DATA"  // a chunk of krec bytes (base64)
	// Savestate keyframe sent to a joining spectator (chunked) before the krec
	// tail, so it can restore to the keyframe's frame and fast-forward only the tail.
	TypeSpectateKeyframe = "SPECTATE_KEYFRAME"
	TypeSpectateEnd      = "SPECTATE_END"  // stream ended (match over / broadcaster left)
	TypeSpectateFail     = "SPECTATE_FAIL" // request rejected (not broadcasting, ended)
	// Sent to every match participant and active spectator whenever the audience changes.
	TypeBroadcastViewerCount = "BROADCAST_VIEWER_COUNT"

	// Moderation (server → client).
	TypeAdminAuthOK   = "ADMIN_AUTH_OK"   // moderator role granted
	TypeAdminAuthFail = "ADMIN_AUTH_FAIL" // wrong password / no admins configured
	TypeModNotice     = "MOD_NOTICE"      // system notice to a user (blocked/muted/kicked/info)
	TypeModList       = "MOD_LIST"        // reply to a moderator's "list" action
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
	ReasonKickedFromRoom  = "kicked_from_room"
)

// -- Request payloads --

type HelloRequest struct {
	Username      string   `json:"username"`
	ClientVersion string   `json:"clientVersion"`
	RomHashes     []string `json:"romHashes"`
	LocalIP       string   `json:"localIp,omitempty"`
	// Self-reported transport medium ("wifi", "lan", "cellular", ...) so peers
	// can gauge connection-quality risk Slippi-style. Optional; older clients
	// simply omit it.
	Connection string `json:"connection,omitempty"`
	// Standard (non-DST) UTC offset in seconds, used to split the North
	// America country bucket into east/central/west. Optional.
	TzOffsetSeconds int `json:"tzOffset,omitempty"`
	// User opted out of location display: presence withholds their country
	// and relays this flag so clients suppress the region badge fallback too.
	// Region still flows for ping estimation.
	Anonymous bool `json:"anonymous,omitempty"`
	// Advertises support for the room-scoped libjuice transport. Older clients
	// omit this and are prevented from entering an ICE match with newer clients.
	ICESupported bool `json:"iceSupported,omitempty"`
}

type HeartbeatRequest struct {
	PingToServer uint16 `json:"pingToServer,omitempty"`
	// True while the client has seen no keyboard/mouse input for a while.
	// The server flips idle/browsing users to the "away" presence state on it;
	// users who are seated, searching, or playing keep their real state.
	Away bool `json:"away,omitempty"`
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

// RoomSwapSlotsRequest exchanges the seats (P1-P4) of two slots. Host-only and
// only while the room is waiting. Either slot may be empty (the occupied player
// just moves to the other slot). The server rebroadcasts ROOM_STATE after.
type RoomSwapSlotsRequest struct {
	SlotA int `json:"slotA"`
	SlotB int `json:"slotB"`
}

// RoomUpdateSettingsRequest is retained for older host-controlled clients.
// Current clients publish per-player delay and host-owned prediction through
// separate messages.
type RoomUpdateSettingsRequest struct {
	Delay          int  `json:"delay"`
	Prediction     int  `json:"prediction"`
	Pacing         int  `json:"pacing,omitempty"`
	DelayAuto      bool `json:"delayAuto"`
	PredictionAuto bool `json:"predictionAuto"`
}

// RoomUpdateDelayRequest publishes the caller's resolved local input delay.
// Every seated player owns this value independently; the server only validates,
// stores it on that player's seat, and rebroadcasts ROOM_STATE.
type RoomUpdateDelayRequest struct {
	Delay     int  `json:"delay"`
	DelayAuto bool `json:"delayAuto"`
}

// RoomUpdatePlayerSettingsRequest is retained for compatibility with clients
// that published both values per seat. Delay remains per-player; Prediction is
// consumed only when the caller is the host and becomes room state.
type RoomUpdatePlayerSettingsRequest struct {
	Delay          int  `json:"delay"`
	DelayAuto      bool `json:"delayAuto"`
	Prediction     int  `json:"prediction"`
	PredictionAuto bool `json:"predictionAuto"`
}

// RoomUpdatePredictionRequest changes the room-wide prediction window. Only
// the host may update it, and only while the room is waiting.
type RoomUpdatePredictionRequest struct {
	Prediction     int  `json:"prediction"`
	PredictionAuto bool `json:"predictionAuto"`
}

// RoomUpdateLiveReplayRequest changes whether the host intends to stream the
// next match. Host-only and waiting-room-only; the server rebroadcasts the
// authoritative choice in ROOM_STATE so every seated player can see it.
type RoomUpdateLiveReplayRequest struct {
	Enabled bool `json:"enabled"`
}

type PingProbeRequestPayload struct {
	TargetUserID uint64 `json:"targetUserId"`
}

type MatchPeerEventRequest struct {
	MatchID    uint64 `json:"matchId"`
	PeerUserID uint64 `json:"peerUserId,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type ICESignalRequest struct {
	TargetUserID uint64 `json:"targetUserId"`
	RoomID       uint64 `json:"roomId"`
	Kind         string `json:"kind"`
	Value        string `json:"value,omitempty"`
	Generation   uint32 `json:"generation"`
}

// QuickMatchJoinRequest scopes a quick-match search to a specific ROM. The
// matchmaker pairs only searchers whose Rom.MD5 agree; Rom.Name is carried so
// the auto-created room can be resolved by name on both clients.
type QuickMatchJoinRequest struct {
	Rom RomInfo `json:"rom"`
}

// -- Response payloads --

type HelloOKResponse struct {
	UserID     uint64           `json:"userId"`
	ObservedIP string           `json:"observedIp"`
	UDPAnchor  string           `json:"udpAnchor"`
	Region     string           `json:"region"`
	ICE        ICEConfiguration `json:"ice"`
}

type ICETransportServer struct {
	Host     string `json:"host"`
	Port     uint16 `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type ICEConfiguration struct {
	STUN ICETransportServer   `json:"stun"`
	TURN []ICETransportServer `json:"turn"`
}

type HelloFailResponse struct {
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
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
	// Sender was authenticated as a moderator (ADMIN_AUTH) when sending.
	// Clients use this to trust admin-only chat directives like "@notify".
	Admin bool `json:"admin,omitempty"`
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
	Country         string `json:"country,omitempty"`
	ClientVersion   string `json:"clientVersion,omitempty"`
	Anonymous       bool   `json:"anonymous,omitempty"` // location withheld at the user's request
	Connection      string `json:"connection,omitempty"`
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
	ViewerCount  int      `json:"viewerCount,omitempty"`  // live replay audience; omitted at zero
}

type RoomListPayload struct {
	Rooms []RoomSummary `json:"rooms"`
}

type RoomSeatPayload struct {
	UserID     uint64 `json:"userId"`
	Username   string `json:"username"`
	Slot       int    `json:"slot"`
	FrameDelay int    `json:"frameDelay"`
	DelayAuto  bool   `json:"delayAuto"`
	// Compatibility mirrors of the host-owned room prediction. New clients use
	// RoomStatePayload.Prediction and PredictionAuto directly.
	PredictionWindow int  `json:"predictionWindow"`
	PredictionAuto   bool `json:"predictionAuto"`
	ICESupported     bool `json:"iceSupported,omitempty"`
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
	// DelayAuto is retained for older clients. PredictionAuto describes the
	// host-owned room prediction selection (Default versus an explicit value).
	DelayAuto         bool              `json:"delayAuto"`
	PredictionAuto    bool              `json:"predictionAuto"`
	LiveReplayEnabled bool              `json:"liveReplayEnabled"`
	Players           []RoomSeatPayload `json:"players"`
	State             string            `json:"state"`
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

// PingProbeIncomingPayload names the peer that wants to be measured and the
// endpoint to probe back. FromEndpoint is always the server's *observed* UDP
// source for that user (set in udp/anchor.go from the packet address), never
// anything the client declared — so this can't be used to aim probes at a
// third party.
type PingProbeIncomingPayload struct {
	FromUserID   uint64 `json:"fromUserId"`
	FromEndpoint string `json:"fromEndpoint"`
}

type MatchPeer struct {
	UserID       uint64 `json:"userId"`
	Username     string `json:"username"`
	PublicIP     string `json:"publicIp"`
	PublicPort   uint16 `json:"publicPort"`
	LocalIP      string `json:"localIp,omitempty"`
	Slot         int    `json:"slot"`
	ICESupported bool   `json:"iceSupported,omitempty"`
}

type MatchBeginPayload struct {
	MatchID uint64      `json:"matchId"`
	Peers   []MatchPeer `json:"peers"`
}

type ICESignalPayload struct {
	FromUserID uint64 `json:"fromUserId"`
	RoomID     uint64 `json:"roomId"`
	Kind       string `json:"kind"`
	Value      string `json:"value,omitempty"`
	Generation uint32 `json:"generation"`
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
// krec bytes travel as base64 in the Data field. The spectator downlink adds a
// server-owned logical offset so receivers can reject gaps and duplicates.

type BroadcastBeginRequest struct {
	MatchID  uint64 `json:"matchId"`
	Manifest string `json:"manifest,omitempty"` // base64 player prematch manifest
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
	MatchID  uint64 `json:"matchId"`
	Manifest string `json:"manifest,omitempty"` // same manifest, before keyframe/input data
}

type SpectateDataPayload struct {
	MatchID uint64 `json:"matchId"`
	Data    string `json:"data"`   // base64 krec chunk
	Frame   int    `json:"frame"`  // broadcaster's live krec frame (fast-forward target)
	Offset  int64  `json:"offset"` // logical byte offset in this spectator's krec stream
}

type SpectateEndPayload struct {
	MatchID uint64 `json:"matchId"`
	Reason  string `json:"reason,omitempty"`
}

type SpectateFailPayload struct {
	MatchID uint64 `json:"matchId"`
	Reason  string `json:"reason"`
}

type BroadcastViewerCountPayload struct {
	MatchID     uint64 `json:"matchId"`
	ViewerCount int    `json:"viewerCount"`
}

// BroadcastKeyframeRequest carries one chunk of a periodic savestate keyframe.
// Frame is the krec input-record index to consume immediately after restoring
// the savestate. The savestate (compressed) is split into
// ChunkCount chunks to stay under the server's per-message read limit; the server
// reassembles them and keeps only the latest complete keyframe.
type BroadcastKeyframeRequest struct {
	MatchID    uint64 `json:"matchId"`
	Frame      int    `json:"frame"`
	ChunkIndex int    `json:"chunkIndex"`
	ChunkCount int    `json:"chunkCount"`
	Data       string `json:"data"` // base64 savestate slice
}

// SpectateKeyframePayload delivers the latest keyframe (chunked) to a joining
// spectator. After reassembling it, the spectator restores the savestate and the
// SPECTATE_DATA that follows begins with input record Frame.
type SpectateKeyframePayload struct {
	MatchID    uint64 `json:"matchId"`
	Frame      int    `json:"frame"`
	ChunkIndex int    `json:"chunkIndex"`
	ChunkCount int    `json:"chunkCount"`
	Data       string `json:"data"` // base64 savestate slice
}

// -- Moderation payloads --

// AdminAuthRequest claims the moderator role with a shared/admin password.
type AdminAuthRequest struct {
	Password string `json:"password"`
}

// AdminAuthOKResponse confirms moderator role and echoes the admin's name.
type AdminAuthOKResponse struct {
	Name string `json:"name"`
}

// ModActionRequest is a moderator command. Action is one of:
// "kick", "mute", "timeout", "ban", "unban", "unmute", "list".
// Target is a username for kick/mute/timeout/ban, or an IP for unban/unmute.
// Duration is a Go duration string ("10m", "1h", "30s"); empty/"0" = permanent.
type ModActionRequest struct {
	Action   string `json:"action"`
	Target   string `json:"target"`
	Duration string `json:"duration,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// ModNoticePayload is a system message shown to a user (e.g. "message blocked",
// "you were muted", or a moderator's command result). Severity is "info",
// "warn", or "error" for client-side styling.
type ModNoticePayload struct {
	Text     string `json:"text"`
	Severity string `json:"severity"`
}

// ModSanction is one ban/mute entry surfaced to a moderator via MOD_LIST.
type ModSanction struct {
	IP     string `json:"ip"`
	Reason string `json:"reason"`
	By     string `json:"by"`
	Until  int64  `json:"until"` // unix ms; 0 = permanent
}

// ModListReply returns the current bans and mutes to a moderator.
type ModListReply struct {
	Bans  []ModSanction `json:"bans"`
	Mutes []ModSanction `json:"mutes"`
}
