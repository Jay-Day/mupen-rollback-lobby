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
	TypeRoomUpdateLateJoin       = "ROOM_UPDATE_LATE_JOIN"
	TypeRoomUpdateSaveData       = "ROOM_UPDATE_SAVE_DATA"
	TypeRoomUpdateJoinLimits     = "ROOM_UPDATE_JOIN_LIMITS"
	TypeRoomSwapSlots            = "ROOM_SWAP_SLOTS"
	TypePingProbeRequest         = "PING_PROBE_REQUEST"
	TypeMatchConnected           = "MATCH_CONNECTED"
	TypeMatchPunchFailed         = "MATCH_PUNCH_FAILED"
	TypeMatchFinished            = "MATCH_FINISHED"
	TypeQuickMatchJoin           = "QUICK_MATCH_JOIN"
	TypeQuickMatchCancel         = "QUICK_MATCH_CANCEL"
	// Local players: extra lobby users sharing this connection. Added and
	// removed from the lobby; they all take seats on room create or join.
	TypeLocalPlayerAdd    = "LOCAL_PLAYER_ADD"
	TypeLocalPlayerRemove = "LOCAL_PLAYER_REMOVE"
	// ICE signaling is relayed over the authenticated lobby WebSocket. The
	// server never sees gameplay datagrams and is not a TURN relay.
	TypeICESignal = "ICE_SIGNAL"

	// Late join: the host adds a room member who isn't in the running match.
	// MATCH_ADMIT starts it; every later step of the hand-off travels as a
	// MATCH_ADMIT_SIGNAL that the server checks and routes (see latejoin.go).
	TypeMatchAdmit       = "MATCH_ADMIT"
	TypeMatchAdmitSignal = "MATCH_ADMIT_SIGNAL"

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
	TypeAdminAuth   = "ADMIN_AUTH"   // claim moderator role with a password
	TypeAdminLogout = "ADMIN_LOGOUT" // give the moderator role back
	TypeModAction   = "MOD_ACTION"   // moderator: kick/mute/timeout/ban/unban/unmute/list/inspect

	// Challenges: asking someone to play right now. Sent from a room it's an
	// invite to that room; otherwise accepting makes a private room for both.
	TypeChallengeSend    = "CHALLENGE_SEND"
	TypeChallengeCancel  = "CHALLENGE_CANCEL"
	TypeChallengeRespond = "CHALLENGE_RESPOND"
	TypeChallengeGames   = "CHALLENGE_GAMES" // which of my games does this player have?
	// Whether this PC takes challenges at all. Everyone sees the choice in
	// presence, so nobody sends one that would only be refused.
	TypeChallengeSettings = "CHALLENGE_SETTINGS"
)

// Server → Client message types
const (
	TypeHelloOK          = "HELLO_OK"
	TypeHelloFail        = "HELLO_FAIL"
	TypeHeartbeatAck     = "HEARTBEAT_ACK"
	TypeChatMsg          = "CHAT_MSG"
	TypeChatFail         = "CHAT_FAIL" // a private message couldn't be delivered
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
	// Replies to LOCAL_PLAYER_ADD / LOCAL_PLAYER_REMOVE, only to the owner.
	TypeLocalPlayerAdded   = "LOCAL_PLAYER_ADDED"
	TypeLocalPlayerRemoved = "LOCAL_PLAYER_REMOVED"
	TypeLocalPlayerFail    = "LOCAL_PLAYER_FAIL"
	// Challenges (server → client). CHALLENGE_SENT acknowledges the sender and
	// CHALLENGE_INCOMING reaches the player it's for; CHALLENGE_RESULT ends it for
	// both of them, or refuses a CHALLENGE_SEND.
	TypeChallengeSent       = "CHALLENGE_SENT"
	TypeChallengeIncoming   = "CHALLENGE_INCOMING"
	TypeChallengeResult     = "CHALLENGE_RESULT"
	TypeChallengeGamesReply = "CHALLENGE_GAMES_REPLY"
	// Late join (server → client). MATCH_ADMIT_BEGIN goes to the match's
	// players and the joiner; MATCH_ADMIT_FAIL answers a refused MATCH_ADMIT.
	TypeMatchAdmitBegin = "MATCH_ADMIT_BEGIN"
	TypeMatchAdmitFail  = "MATCH_ADMIT_FAIL"

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
	TypeAdminAuthOK    = "ADMIN_AUTH_OK"    // moderator role granted
	TypeAdminAuthFail  = "ADMIN_AUTH_FAIL"  // wrong password / no admins configured
	TypeAdminLoggedOut = "ADMIN_LOGGED_OUT" // moderator role given back
	TypeModNotice      = "MOD_NOTICE"       // system notice to a user (blocked/muted/kicked/info)
	TypeModList        = "MOD_LIST"         // reply to a moderator's "list" action
	TypeModResult      = "MOD_RESULT"       // a kick/mute/timeout/ban took effect
	TypeModTargetInfo  = "MOD_TARGET_INFO"  // reply to a moderator's "inspect" action
	TypeModPeers       = "MOD_PEERS"        // who is a moderator, to moderators
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
	// Late join refusals and aborts.
	ReasonLateJoinUnsupported = "late_join_unsupported"
	ReasonAdmitBusy           = "admit_busy"
	ReasonNotWaiting          = "not_waiting"
	ReasonJoinerLeft          = "joiner_left"
	// Local player refusals.
	ReasonNotEnoughSeats  = "not_enough_seats"  // fewer free seats than players on this PC
	ReasonNeedsUpdate     = "needs_update"      // this client can't share a room with local players
	ReasonPeerNeedsUpdate = "peer_needs_update" // someone seated can't share a room with local players
	ReasonLocalPlayers    = "local_players"     // Quick Match takes one player per PC
	ReasonInvalidName     = "invalid_name"
	ReasonNameTaken       = "name_taken"
	ReasonTooManyPlayers  = "too_many"
	ReasonInRoom          = "in_room"
	ReasonSearching       = "searching"
	ReasonTooFast         = "too_fast"
	// Refusals for private messages (ReasonNeedsUpdate too).
	ReasonUserGone = "user_gone"
	ReasonSamePC   = "same_pc"
	// Refused joining a room made for a challenge.
	ReasonPrivateRoom = "private_room"
	// Refused by the room's join limits: someone on the joining PC is on
	// Wi-Fi, or the joiner's ping to the host is over the room's maximum.
	ReasonWiredOnly   = "wired_only"
	ReasonPingTooHigh = "ping_too_high"
)

// How a challenge ended, or why CHALLENGE_SEND was refused (ReasonNeedsUpdate
// and ReasonTooFast too). ChallengeUnavailable says no more than that on purpose:
// the real reason may be how many players are on someone's PC.
const (
	ChallengeAccepted    = "accepted"
	ChallengeDeclined    = "declined"
	ChallengeTimedOut    = "timeout"
	ChallengeCancelled   = "cancelled"
	ChallengeBusy        = "busy"           // in a match
	ChallengeUnavailable = "unavailable"    // can't happen right now
	ChallengeNoGame      = "no_game"        // they don't have the game
	ChallengeCooldown    = "cooldown"       // they declined you moments ago
	ChallengePending     = "pending"        // you already have one out
	ChallengeBlocked     = "blocked_note"   // the message hit the chat filter
	ChallengeOff         = "challenges_off" // they don't take challenges
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
	// Advertises support for joining a running match (MATCH_ADMIT_*). A match
	// only sizes itself for late joiners when every seated player sets it.
	LateJoin bool `json:"lateJoin,omitempty"`
	// Advertises support for local players: the client routes by each seat's
	// PeerID. A room never mixes clients that don't with a PC holding several
	// players, since they'd open a connection per seat.
	LocalPlayers bool `json:"localPlayers,omitempty"`
	// Advertises support for private messages and challenges. Peers are told
	// through presence, so they don't offer them to a client that can't show them.
	Social bool `json:"social,omitempty"`
	// Advertises that this client shows a match's players their spectators' chat,
	// and tells spectators the players can read it. A spectator without it was
	// promised they can't, so the players don't get that spectator's lines.
	SpectatorChat bool `json:"spectatorChat,omitempty"`
	// Advertises support for matches that start from the host's save data.
	// Such a match can't start, or take a late joiner, without it.
	SaveData bool `json:"saveData,omitempty"`
	// This PC doesn't take challenges (CHALLENGE_SETTINGS changes it later).
	// Sent here too, so no challenge can slip in before the first update.
	NoChallenges bool `json:"noChallenges,omitempty"`
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
	// Private messages name who they're for. AsUserID picks who on this PC is
	// talking; zero means the connection's owner.
	ToUserID uint64 `json:"toUserId,omitempty"`
	AsUserID uint64 `json:"asUserId,omitempty"`
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
	// Join limits (see RoomJoinLimitsPayload). Zero and false take anyone.
	MaxPing   int  `json:"maxPing,omitempty"`
	WiredOnly bool `json:"wiredOnly,omitempty"`
}

type RomInfo struct {
	Name   string `json:"name"`
	MD5    string `json:"md5"`
	Region string `json:"region"`
}

type RoomJoinRequest struct {
	RoomID   uint64 `json:"roomId"`
	Password string `json:"password,omitempty"`
	// The joiner's ping to the host in ms: measured if it can be, otherwise
	// estimated from regions. Zero when unknown. Only a room with a maximum
	// ping reads it; the server has no way to measure it itself.
	PingMs int `json:"pingMs,omitempty"`
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

// RoomUpdateLateJoinRequest changes whether the next match can take players
// mid-game (MATCH_ADMIT). Host-only and waiting-room-only; the server
// rebroadcasts the choice in ROOM_STATE as LateJoinEnabled.
type RoomUpdateLateJoinRequest struct {
	Enabled bool `json:"enabled"`
}

// RoomUpdateSaveDataRequest turns save data on or off for the room's next
// match and describes the host's pick. Host-only and waiting-room-only. The
// save itself goes from the host to the players in the pre-match manifest;
// the server only relays this description in ROOM_STATE.
type RoomUpdateSaveDataRequest struct {
	Enabled bool     `json:"enabled"`
	Label   string   `json:"label,omitempty"`
	Kinds   []string `json:"kinds,omitempty"`
	Bytes   int      `json:"bytes,omitempty"`
	Hash    string   `json:"hash,omitempty"`
}

// RoomUpdateJoinLimitsRequest changes who may join the room from now on.
// Host-only, in any state; players already seated stay.
type RoomUpdateJoinLimitsRequest struct {
	MaxPing   int  `json:"maxPing"`
	WiredOnly bool `json:"wiredOnly"`
}

// RoomJoinLimitsPayload is ROOM_STATE's view of the room's join limits.
// MaxPing is the highest ping to the host a joiner may have, in ms (0 takes
// any); WiredOnly turns away a PC whose player is on Wi-Fi or cellular.
type RoomJoinLimitsPayload struct {
	MaxPing   int  `json:"maxPing"`
	WiredOnly bool `json:"wiredOnly"`
}

// RoomSaveDataPayload is ROOM_STATE's view of the room's save data option.
type RoomSaveDataPayload struct {
	Enabled bool     `json:"enabled"`
	Label   string   `json:"label,omitempty"`
	Kinds   []string `json:"kinds,omitempty"`
	Bytes   int      `json:"bytes,omitempty"`
	Hash    string   `json:"hash,omitempty"`
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

// LocalPlayerAddRequest adds a player to this connection. Refused while in a
// room or searching.
type LocalPlayerAddRequest struct {
	Name string `json:"name"`
}

type LocalPlayerRemoveRequest struct {
	UserID uint64 `json:"userId"`
}

// ChallengeSendRequest asks a player to play. Rom is ignored from inside a
// room, where this invites them to it.
type ChallengeSendRequest struct {
	ToUserID   uint64  `json:"toUserId"`
	Rom        RomInfo `json:"rom"`
	Note       string  `json:"note,omitempty"`
	LiveReplay bool    `json:"liveReplay,omitempty"`
}

type ChallengeCancelRequest struct {
	ChallengeID uint64 `json:"challengeId"`
}

type ChallengeRespondRequest struct {
	ChallengeID uint64 `json:"challengeId"`
	Accept      bool   `json:"accept"`
}

type ChallengeGamesRequest struct {
	ToUserID uint64 `json:"toUserId"`
}

// ChallengeSettingsRequest turns challenges to this PC off or back on. Off
// also ends the ones waiting for an answer.
type ChallengeSettingsRequest struct {
	Off bool `json:"off"`
}

// -- Response payloads --

type HelloOKResponse struct {
	UserID uint64 `json:"userId"`
	// Username is the spelling the lobby kept, which can differ from HELLO's.
	Username   string           `json:"username"`
	ObservedIP string           `json:"observedIp"`
	UDPAnchor  string           `json:"udpAnchor"`
	Region     string           `json:"region"`
	ICE        ICEConfiguration `json:"ice"`
	// LocalPlayers tells the client LOCAL_PLAYER_ADD works here.
	LocalPlayers bool `json:"localPlayers,omitempty"`
	// Social tells the client private messages and challenges work here.
	Social bool `json:"social,omitempty"`
	// SpectatorChat tells the client a match's players share its spectators' chat here.
	SpectatorChat bool `json:"spectatorChat,omitempty"`
	// ChallengeSettings tells the client CHALLENGE_SETTINGS works here.
	ChallengeSettings bool `json:"challengeSettings,omitempty"`
}

type LocalPlayerAddedPayload struct {
	UserID uint64 `json:"userId"`
	Name   string `json:"name"`
}

type LocalPlayerRemovedPayload struct {
	UserID uint64 `json:"userId"`
}

type LocalPlayerFailPayload struct {
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason"`
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
	// Spectator chat only: the sender is playing the match being watched.
	FromPlayer bool `json:"fromPlayer,omitempty"`
	// Private messages only: who it's for. It may be a local player, whose
	// owner's connection receives it.
	ToUserID   uint64 `json:"toUserId,omitempty"`
	ToUsername string `json:"toUsername,omitempty"`
}

// ChatFailPayload says why a private message went nowhere.
type ChatFailPayload struct {
	Channel  string `json:"channel"`
	ToUserID uint64 `json:"toUserId"`
	Reason   string `json:"reason"`
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
	Social          bool   `json:"social,omitempty"`       // can receive private messages and challenges
	NoChallenges    bool   `json:"noChallenges,omitempty"` // their PC doesn't take challenges
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
	// LateJoin marks an in-game room with a free seat that a late-join capable
	// client may enter; the host can then add them to the running match.
	LateJoin bool `json:"lateJoin,omitempty"`
	// Private rooms were made for a challenge and refuse ROOM_JOIN.
	Private bool `json:"private,omitempty"`
	// Join limits, so the list can say who a room takes before anyone tries.
	MaxPing   int  `json:"maxPing,omitempty"`
	WiredOnly bool `json:"wiredOnly,omitempty"`
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
	// InMatch is true while this seat plays in the room's running match. A
	// member who joined mid-match (or dropped) is seated with InMatch false.
	// Always sent: clients read a missing value (an older server) as true.
	InMatch bool `json:"inMatch"`
	// LateJoin mirrors the client's capability so the host's UI can tell
	// whether it can add this member to a running match.
	LateJoin bool `json:"lateJoin,omitempty"`
	// PeerID is the user whose connection this seat plays through: UserID,
	// unless the seat is a local player. Clients connect per PeerID.
	PeerID uint64 `json:"peerId,omitempty"`
	// SaveData mirrors the client's capability, so the host can see who
	// keeps a save data match from starting.
	SaveData bool `json:"saveData,omitempty"`
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
	// InGameSettings tells clients that ROOM_UPDATE_DELAY and
	// ROOM_UPDATE_PREDICTION are also accepted while the room is in game.
	// Older servers drop them, so clients keep the host's mid-match
	// prediction control disabled unless this is set.
	InGameSettings bool `json:"inGameSettings"`
	// LateJoin is set while the room's running match can take more players:
	// it was sized for every seat (MatchBeginPayload.Seats) and the host may
	// add waiting members with MATCH_ADMIT.
	LateJoin bool `json:"lateJoin,omitempty"`
	// CanStartAlone tells the host ROOM_START works with nobody else seated.
	// Older servers refuse it, so clients need this to offer the option.
	CanStartAlone bool `json:"canStartAlone,omitempty"`
	Private       bool `json:"private,omitempty"`
	// LateJoinEnabled is the host's choice (ROOM_UPDATE_LATE_JOIN, off in a
	// new room): only then is the next match sized for late joiners. Sent for
	// every room that has the option (not quick match rooms), so clients can
	// tell which rooms offer it.
	LateJoinEnabled *bool `json:"lateJoinEnabled,omitempty"`
	// SaveData is the host's choice of starting save (ROOM_UPDATE_SAVE_DATA,
	// off in a new room). Sent for every room that has the option (not quick
	// match rooms), so clients can tell which rooms offer it.
	SaveData *RoomSaveDataPayload `json:"saveData,omitempty"`
	// JoinLimits says who may join (ROOM_UPDATE_JOIN_LIMITS). Sent for every
	// room that has them (not quick match rooms), so clients can tell which
	// rooms offer the option.
	JoinLimits *RoomJoinLimitsPayload `json:"joinLimits,omitempty"`
}

type RoomJoinFailPayload struct {
	Reason string `json:"reason"`
}

// RoomLeftPayload tells the client why it left a room: "left" (self),
// "kicked" (removed by host), or "host_left" (host dissolved the room).
type RoomLeftPayload struct {
	Reason string `json:"reason,omitempty"`
}

// ChallengeSentPayload acknowledges a CHALLENGE_SEND. ExpiresInMs counts from
// now, so clients don't depend on their clocks agreeing with the server's.
type ChallengeSentPayload struct {
	ChallengeID uint64  `json:"challengeId"`
	ToUserID    uint64  `json:"toUserId"`
	ToUsername  string  `json:"toUsername"`
	Rom         RomInfo `json:"rom"`
	ExpiresInMs int64   `json:"expiresInMs"`
	Invite      bool    `json:"invite,omitempty"`
}

// ChallengeIncomingPayload reaches the challenged player's connection. ToUserID
// may be a local player. An invite names the room instead of proposing a new one.
type ChallengeIncomingPayload struct {
	ChallengeID  uint64  `json:"challengeId"`
	FromUserID   uint64  `json:"fromUserId"`
	FromUsername string  `json:"fromUsername"`
	ToUserID     uint64  `json:"toUserId"`
	ToUsername   string  `json:"toUsername"`
	Rom          RomInfo `json:"rom"`
	Note         string  `json:"note,omitempty"`
	ExpiresInMs  int64   `json:"expiresInMs"`
	Invite       bool    `json:"invite,omitempty"`
	RoomName     string  `json:"roomName,omitempty"`
	RoomPlayers  int     `json:"roomPlayers,omitempty"`
	RoomMax      int     `json:"roomMaxPlayers,omitempty"`
}

// ChallengeResultPayload ends a challenge, or refuses a CHALLENGE_SEND, in which
// case ChallengeID is zero and ToUserID says which one.
type ChallengeResultPayload struct {
	ChallengeID uint64 `json:"challengeId,omitempty"`
	ToUserID    uint64 `json:"toUserId,omitempty"`
	Result      string `json:"result"`
}

// ChallengeGamesReply lists the sender's games, by MD5, that the player has too.
type ChallengeGamesReply struct {
	ToUserID uint64   `json:"toUserId"`
	MD5s     []string `json:"md5s"`
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
	// PeerID is the user whose connection this seat plays through (see
	// RoomSeatPayload.PeerID).
	PeerID uint64 `json:"peerId,omitempty"`
}

type MatchBeginPayload struct {
	MatchID uint64      `json:"matchId"`
	Peers   []MatchPeer `json:"peers"`
	// Seconds between the live replay broadcaster's savestate keyframes, from
	// the lobby config. Clients use 30 when it's missing (older lobbies).
	KeyframeIntervalSec int `json:"keyframeIntervalSec,omitempty"`
	// Seats is the number of controller ports every client emulates. It is
	// the room's capacity when every seated player supports late join, so an
	// empty seat is an idle controller someone can take over mid-match. Zero
	// keeps the older sizing (the highest occupied seat).
	Seats int `json:"seats,omitempty"`
}

// MatchAdmitRequest is the host asking to add a waiting room member to the
// running match.
type MatchAdmitRequest struct {
	MatchID uint64 `json:"matchId"`
	UserID  uint64 `json:"userId"`
}

type MatchAdmitFailPayload struct {
	MatchID uint64 `json:"matchId"`
	UserID  uint64 `json:"userId"`
	Reason  string `json:"reason"`
}

// MatchAdmitBeginPayload starts a hand-off. Peers is the roster after the
// join (the joiner included); Epoch numbers the rollback session the clients
// switch to, so packets of the session being replaced can't reach it.
type MatchAdmitBeginPayload struct {
	MatchID uint64      `json:"matchId"`
	AdmitID uint64      `json:"admitId"`
	Epoch   int         `json:"epoch"`
	Seats   int         `json:"seats"`
	HostID  uint64      `json:"hostId"`
	Joiner  MatchPeer   `json:"joiner"`
	Peers   []MatchPeer `json:"peers"`
	// Joiners is every player the hand-off adds, Joiner first: a PC joins with
	// all of its local players.
	Joiners []MatchPeer `json:"joiners,omitempty"`
}

// Late join hand-off steps, in order. Each names who may send it and who
// receives it (the server enforces both).
const (
	AdmitStepManifest = "manifest"  // host → joiner: pre-match settings (chunked)
	AdmitStepReady    = "ready"     // joiner → host: booted and connected to every player
	AdmitStepCut      = "cut"       // host → players: stop at Frame
	AdmitStepCutReady = "cut_ready" // player → host: stopped at Frame (OK false: already past it)
	AdmitStepState    = "state"     // host → joiner: the game state at the cut (chunked)
	AdmitStepGo       = "go"        // host → players and joiner: switch to the new session
	AdmitStepAbort    = "abort"     // anyone involved (or the server) → everyone else
)

// MatchAdmitSignalPayload is one hand-off step. Data carries a base64 chunk
// for the chunked steps; Index/Count place it.
type MatchAdmitSignalPayload struct {
	MatchID    uint64 `json:"matchId"`
	AdmitID    uint64 `json:"admitId"`
	Step       string `json:"step"`
	FromUserID uint64 `json:"fromUserId,omitempty"`
	Frame      int    `json:"frame,omitempty"`
	OK         bool   `json:"ok,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Index      int    `json:"index,omitempty"`
	Count      int    `json:"count,omitempty"`
	Data       string `json:"data,omitempty"`
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
	// Reason says why a QUICK_MATCH_JOIN was refused.
	Reason string `json:"reason,omitempty"`
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
// "kick", "mute", "timeout", "ban", "unban", "unmute", "list", "inspect",
// "closeroom".
// Target is a username for kick/mute/timeout/ban/inspect, or an IP for
// unban/unmute.
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
	Name   string `json:"name,omitempty"` // who it was applied to; empty on older entries
	Reason string `json:"reason"`
	By     string `json:"by"`
	Until  int64  `json:"until"` // unix ms; 0 = permanent
}

// ModListReply returns the current bans and mutes to a moderator.
type ModListReply struct {
	Bans  []ModSanction `json:"bans"`
	Mutes []ModSanction `json:"mutes"`
}

// ModResultPayload confirms a sanction with what's needed to undo it. It goes
// with the MOD_NOTICE text older clients show.
type ModResultPayload struct {
	Action string `json:"action"`
	Target string `json:"target"`
	IP     string `json:"ip,omitempty"`
	Until  int64  `json:"until,omitempty"` // unix ms; 0 = permanent
}

// ModTargetInfoPayload answers "inspect": what acting on Target would affect.
// Others is everyone else on the same connection. Moderators are the only people
// ever told that a PC is shared.
type ModTargetInfoPayload struct {
	Target    string   `json:"target"`
	Others    []string `json:"others,omitempty"`
	Moderator bool     `json:"moderator,omitempty"`
}

// ModPeersPayload lists the users moderators can't act on. Local players of a
// moderator's connection are included. Only moderators receive it.
type ModPeersPayload struct {
	UserIDs []uint64 `json:"userIds"`
}
