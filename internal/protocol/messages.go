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
	// Host-only, waiting-room-only: turn ranked on/off and pick its settings.
	TypeRoomUpdateRanked = "ROOM_UPDATE_RANKED"
	TypePingProbeRequest = "PING_PROBE_REQUEST"
	TypeMatchConnected   = "MATCH_CONNECTED"
	TypeMatchPunchFailed = "MATCH_PUNCH_FAILED"
	TypeMatchFinished    = "MATCH_FINISHED"
	// Each ranked player reports the finished set; the server rates it once
	// every report agrees.
	TypeMatchResult = "MATCH_RESULT"
	// Revokes the remember-me token the client logged in with.
	TypeAccountLogout = "ACCOUNT_LOGOUT"
	// Changes the logged-in account's password; every saved login of the
	// account is revoked and this connection gets a new session token.
	TypeAccountChangePassword = "ACCOUNT_CHANGE_PASSWORD"
	// Link the logged-in account to a Discord account (DiscordCodeRequest),
	// or unlink it; answered with ACCOUNT_DISCORD / ACCOUNT_DISCORD_FAIL.
	TypeAccountLinkDiscord   = "ACCOUNT_LINK_DISCORD"
	TypeAccountUnlinkDiscord = "ACCOUNT_UNLINK_DISCORD"
	// Account email, only when the server can send email: ACCOUNT_SET_EMAIL
	// emails a code to a new address, ACCOUNT_CONFIRM_EMAIL with that code
	// makes it the account's address, ACCOUNT_REMOVE_EMAIL forgets it.
	// Answered with ACCOUNT_EMAIL_CODE_SENT, ACCOUNT_EMAIL or ACCOUNT_EMAIL_FAIL.
	TypeAccountSetEmail     = "ACCOUNT_SET_EMAIL"
	TypeAccountConfirmEmail = "ACCOUNT_CONFIRM_EMAIL"
	TypeAccountRemoveEmail  = "ACCOUNT_REMOVE_EMAIL"
	// Ranked replays: a player offers the set's .krec (size + hash); only
	// the first offer is accepted, then its bytes follow in chunks.
	TypeReplayOffer      = "REPLAY_OFFER"
	TypeReplayChunk      = "REPLAY_CHUNK"
	TypeQuickMatchJoin   = "QUICK_MATCH_JOIN"
	TypeQuickMatchCancel = "QUICK_MATCH_CANCEL"
	// Answer to QUICK_MATCH_REMATCH_OFFER (QuickMatchRematchRequest).
	TypeQuickMatchRematch = "QUICK_MATCH_REMATCH"
	// Turns a ranked session's "keep searching" on or off, e.g. "stop after
	// this set" (QuickMatchSessionRequest).
	TypeQuickMatchSession = "QUICK_MATCH_SESSION"
	// Turns a 2v2 session's "keep my teammate" on or off
	// (QuickMatchTeammateRequest).
	TypeQuickMatchTeammate = "QUICK_MATCH_TEAMMATE"
	// A client's measured ping to another player in its room
	// (RoomPeerPingRequest), for ranked max-ping checks.
	TypeRoomPeerPing = "ROOM_PEER_PING"
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
	// Ranked matches: the game module's memory write journal (opaque bytes,
	// see the RMG-K ranked host). Spectators apply it alongside the krec
	// inputs, or their replay drifts from the match.
	TypeBroadcastJournal = "BROADCAST_JOURNAL"
	TypeSpectateStart    = "SPECTATE_START" // spectator: begin watching a match
	TypeSpectateStop     = "SPECTATE_STOP"  // spectator: stop watching

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
	// Late join (server → client). MATCH_ADMIT_BEGIN goes to the match's
	// players and the joiner; MATCH_ADMIT_FAIL answers a refused MATCH_ADMIT.
	TypeMatchAdmitBegin = "MATCH_ADMIT_BEGIN"
	TypeMatchAdmitFail  = "MATCH_ADMIT_FAIL"
	// A continuous ranked search found only previous opponents after waiting
	// (QuickMatchRematchOfferPayload); answer with QUICK_MATCH_REMATCH.
	TypeQuickMatchRematchOffer = "QUICK_MATCH_REMATCH_OFFER"
	// Outcome of a ranked set (rated, voided or disputed) with rating changes.
	TypeRankedResult = "RANKED_RESULT"
	// ACCOUNT_CHANGE_PASSWORD outcome.
	TypeAccountPasswordChanged = "ACCOUNT_PASSWORD_CHANGED"
	// The account's Discord link after a link or unlink (DiscordAccountInfo).
	TypeAccountDiscord      = "ACCOUNT_DISCORD"
	TypeAccountDiscordFail  = "ACCOUNT_DISCORD_FAIL"
	TypeAccountPasswordFail = "ACCOUNT_PASSWORD_FAIL"
	// Account email: a code went to the pending address (AccountEmailInfo
	// with that address); the account's address after a confirm or a remove
	// (AccountEmailInfo); or a failure (AccountPasswordFailPayload).
	TypeAccountEmailCodeSent = "ACCOUNT_EMAIL_CODE_SENT"
	TypeAccountEmail         = "ACCOUNT_EMAIL"
	TypeAccountEmailFail     = "ACCOUNT_EMAIL_FAIL"
	// Replay upload flow (see REPLAY_OFFER).
	TypeReplayAccept  = "REPLAY_ACCEPT"
	TypeReplayDecline = "REPLAY_DECLINE"
	TypeReplayStored  = "REPLAY_STORED"

	// Spectating (server → spectator).
	TypeSpectateBegin = "SPECTATE_BEGIN" // ack: stream is starting; history follows
	TypeSpectateData  = "SPECTATE_DATA"  // a chunk of krec bytes (base64)
	// Savestate keyframe sent to a joining spectator (chunked) before the krec
	// tail, so it can restore to the keyframe's frame and fast-forward only the tail.
	TypeSpectateKeyframe = "SPECTATE_KEYFRAME"
	// Write journal chunks, in order and complete from the match start, sent
	// ahead of the krec data they apply to (see BROADCAST_JOURNAL).
	TypeSpectateJournal = "SPECTATE_JOURNAL"
	TypeSpectateEnd     = "SPECTATE_END"  // stream ended (match over / broadcaster left)
	TypeSpectateFail    = "SPECTATE_FAIL" // request rejected (not broadcasting, ended)
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
	// Late join refusals and aborts.
	ReasonLateJoinUnsupported = "late_join_unsupported"
	ReasonAdmitBusy           = "admit_busy"
	ReasonNotWaiting          = "not_waiting"
	ReasonJoinerLeft          = "joiner_left"

	// Accounts
	ReasonAuthFailed          = "auth_failed"          // wrong username/password or expired token
	ReasonUsernameRegistered  = "username_registered"  // guest tried a registered name
	ReasonWeakPassword        = "weak_password"        // registration password too short/long
	ReasonTooManyAttempts     = "too_many_attempts"    // auth rate limit
	ReasonAccountsUnavailable = "accounts_unavailable" // server has no account store
	ReasonDiscordUnavailable  = "discord_unavailable"  // server has no Discord app configured
	ReasonDiscordFailed       = "discord_failed"       // the Discord code exchange or lookup failed
	ReasonDiscordUnlinked     = "discord_unlinked"     // no account linked yet: link with a password (ticket)
	ReasonDiscordInUse        = "discord_in_use"       // that Discord account is linked to another account
	ReasonTicketExpired       = "ticket_expired"       // the Discord link ticket expired or was used
	ReasonEmailUnavailable    = "email_unavailable"    // server has no email (SMTP) configured
	ReasonInvalidEmail        = "invalid_email"        // not a plain address like name@example.com
	ReasonEmailFailed         = "email_failed"         // the email couldn't be sent

	// Ranked rooms
	ReasonRankedUnavailable = "ranked_unavailable" // ROM not on a ladder / ranked not configured
	ReasonNotVerified       = "not_verified"       // ranked needs a logged-in account
	ReasonSkillMismatch     = "skill_mismatch"     // outside the room's skill filter
	ReasonTooFar            = "too_far"            // estimated ping above the room's max ping
	ReasonRankedSeats       = "ranked_seats"       // ranked sets are 1v1
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
	// Optional account authentication; guests omit it.
	Auth *HelloAuth `json:"auth,omitempty"`
	// Ranked game modules the client has installed.
	RankedModules []RankedModuleInfo `json:"rankedModules,omitempty"`
}

// HelloAuth authenticates the HELLO username. Mode is "login" or "register"
// (with Password), or "token" (with a remember-me Token from HELLO_OK; the
// username is then taken from the account).
// HelloAuth authenticates a HELLO. Mode is "login", "register", "token" or
// "reset" (sets Password with a moderator-issued ResetCode).
type HelloAuth struct {
	Mode      string `json:"mode"`
	Password  string `json:"password,omitempty"`
	Token     string `json:"token,omitempty"`
	ResetCode string `json:"resetCode,omitempty"`
	// mode "discord": the authorization code from Discord's redirect, the
	// PKCE verifier and the loopback redirect it was sent to.
	Code         string `json:"code,omitempty"`
	CodeVerifier string `json:"codeVerifier,omitempty"`
	RedirectURI  string `json:"redirectUri,omitempty"`
	// modes "discord_link" (log in) and "discord_register" (create an
	// account): the ticket from a discord_unlinked failure, with
	// the account's username (HELLO username) and password.
	Ticket string `json:"ticket,omitempty"`
}

// DiscordCodeRequest is ACCOUNT_LINK_DISCORD: a Discord authorization code
// for the logged-in account.
type DiscordCodeRequest struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"codeVerifier"`
	RedirectURI  string `json:"redirectUri"`
}

// DiscordAccountInfo is the logged-in account's Discord link (only ever sent
// to that account).
type DiscordAccountInfo struct {
	Linked   bool   `json:"linked"`
	Username string `json:"username,omitempty"`
	Name     string `json:"name,omitempty"`
}

type AccountChangePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// AccountPasswordChangedPayload carries the connection's new session token.
type AccountPasswordChangedPayload struct {
	SessionToken string `json:"sessionToken"`
}

type AccountPasswordFailPayload struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// AccountSetEmailRequest is ACCOUNT_SET_EMAIL: the address to confirm.
type AccountSetEmailRequest struct {
	Email string `json:"email"`
}

// AccountConfirmEmailRequest is ACCOUNT_CONFIRM_EMAIL: the code emailed to
// the new address.
type AccountConfirmEmailRequest struct {
	Code string `json:"code"`
}

// AccountEmailInfo is an account's email address, only ever sent to that
// account. Address is "" when it has none.
type AccountEmailInfo struct {
	Address string `json:"address"`
}

type RankedModuleInfo struct {
	ID      string `json:"id"`
	Version string `json:"version"`
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
	// Optional: create the room as ranked.
	Ranked *RankedRoomRequest `json:"ranked,omitempty"`
}

// RankedRoomRequest is used by ROOM_CREATE and ROOM_UPDATE_RANKED. Zero
// values pick the ladder defaults.
type RankedRoomRequest struct {
	Enabled bool `json:"enabled"`
	// LadderID picks the ladder (e.g. the 2v2 one) among the ROM's ladders;
	// empty = the ROM's 1v1 ladder.
	LadderID  string `json:"ladderId,omitempty"`
	RulesetID string `json:"rulesetId,omitempty"`
	BestOf    int    `json:"bestOf,omitempty"`
	// MaxPing (ms) limits the ping between players: joiners are checked by
	// region estimate, the start by measured ping. 0 = any.
	MaxPing int `json:"maxPing,omitempty"`
	// MaxTierGap limits joiners to within this many tiers of the host;
	// 0 = anyone verified may join.
	MaxTierGap int `json:"maxTierGap,omitempty"`
	// AllowProvisional lets players still in placement join regardless of the
	// tier gap. Defaults to true when omitted.
	AllowProvisional *bool `json:"allowProvisional,omitempty"`
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

// MatchResultRequest is one ranked player's report of the finished set.
// Slots are 1-indexed controller ports, as in MATCH_BEGIN.
type MatchResultRequest struct {
	MatchID    uint64 `json:"matchId"`
	WinnerSlot int    `json:"winnerSlot"`
	Score      []int  `json:"score"` // games won (a team's, in team sets), index 0 = slot 1
	// Team sets: each slot's team (index 0 = slot 1): 1 for the team with
	// the lowest slot, 2 for the other. WinnerTeam is 1 or 2.
	Teams      []int             `json:"teams,omitempty"`
	WinnerTeam int               `json:"winnerTeam,omitempty"`
	Games      []MatchResultGame `json:"games"`
	// Voided games (resets, wrong rules...) that were replayed.
	VoidedGames int `json:"voidedGames,omitempty"`
	// Ruleset hash the client's module computed; must match between players.
	RulesetHash string `json:"rulesetHash"`
	// "ok", or why the client distrusts its own result (e.g. a desync).
	Integrity string `json:"integrity"`
	// What the set was played with; a replay needs the same module version.
	RulesetID     string `json:"rulesetId,omitempty"`
	ModuleVersion string `json:"moduleVersion,omitempty"`
	RomMD5        string `json:"romMd5,omitempty"`
}

// ReplayBookmark marks a position in a replay. Record is the 0-based index
// of the .krec input record to resume from.
type ReplayBookmark struct {
	Record int    `json:"record"`
	Label  string `json:"label"`
	Kind   string `json:"kind"`           // "game_start", "game_end" or "user"
	Game   int    `json:"game,omitempty"` // 1-based game number for game marks
}

type ReplayOfferRequest struct {
	SetID     int64            `json:"setId"`
	Size      int              `json:"size"`
	SHA256    string           `json:"sha256"`
	Bookmarks []ReplayBookmark `json:"bookmarks,omitempty"`
}

type ReplayChunkRequest struct {
	SetID int64  `json:"setId"`
	Index int    `json:"index"`
	Count int    `json:"count"`
	Data  string `json:"data"` // base64
}

type ReplayAcceptPayload struct {
	SetID     int64 `json:"setId"`
	ChunkSize int   `json:"chunkSize"` // raw bytes per chunk before base64
}

type ReplayDeclinePayload struct {
	SetID  int64  `json:"setId"`
	Reason string `json:"reason"` // "exists", "in_progress", "not_participant", "invalid", "unavailable", "failed"
}

type ReplayStoredPayload struct {
	SetID int64 `json:"setId"`
}

type MatchResultGame struct {
	Game       int                 `json:"game"`
	WinnerSlot int                 `json:"winnerSlot"`
	WinnerTeam int                 `json:"winnerTeam,omitempty"` // team sets
	Stage      int                 `json:"stage"`
	Players    []MatchResultPlayer `json:"players"`
}

type MatchResultPlayer struct {
	Slot       int `json:"slot"`
	Character  int `json:"character"`
	StocksLeft int `json:"stocksLeft"`
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
	// Ranked queues for a ranked set (Enabled must be set). Searchers pair
	// only with ranked searchers on the same ladder, roster and set length
	// whose skill filters (MaxTierGap, AllowProvisional) accept each other.
	Ranked *RankedRoomRequest `json:"ranked,omitempty"`
	// Continuous keeps the search going: after each set (ranked) or match
	// (casual: once a player closes the game) the lobby closes the room and
	// searches again with the same settings, avoiding the previous opponent.
	Continuous bool `json:"continuous,omitempty"`
	// Teammate (2v2): the player wanted as teammate. The search waits until
	// that player searches for the same set wanting this one back, then the
	// two are matched as a team. Empty = a random teammate.
	Teammate string `json:"teammate,omitempty"`
	// KeepTeammate (2v2 sessions, with Teammate): after each set, search
	// again together with that set's teammate when they keep the team too.
	// Ignored without Teammate; QUICK_MATCH_TEAMMATE sets it in the room.
	KeepTeammate bool `json:"keepTeammate,omitempty"`
}

// QuickMatchRematchRequest answers a rematch offer: play the previous
// opponent, or keep searching for someone new.
type QuickMatchRematchRequest struct {
	Accept bool `json:"accept"`
}

// QuickMatchSessionRequest turns "keep searching after each set" on or off
// for the current search or quick-match room.
type QuickMatchSessionRequest struct {
	Continuous bool `json:"continuous"`
}

// QuickMatchTeammateRequest turns "keep my teammate" on or off for the
// current 2v2 search or quick-match room. Turned on between sets, it keeps
// the last set's team when the teammate keeps it too; turned off, the team
// splits and both search on their own.
type QuickMatchTeammateRequest struct {
	Keep bool `json:"keep"`
}

// -- Response payloads --

type HelloOKResponse struct {
	UserID     uint64           `json:"userId"`
	ObservedIP string           `json:"observedIp"`
	UDPAnchor  string           `json:"udpAnchor"`
	Region     string           `json:"region"`
	ICE        ICEConfiguration `json:"ice"`
	// Username as registered (canonical casing) when authenticated; clients
	// should display this instead of what they typed.
	Username string `json:"username,omitempty"`
	// Present when the HELLO authenticated an account.
	Account *AccountInfo `json:"account,omitempty"`
	// Ranked ladders this server runs, so clients know which ROMs qualify.
	Ladders []LadderInfo `json:"ladders,omitempty"`
}

type AccountInfo struct {
	AccountID int64 `json:"accountId"`
	// Remember-me token for HELLO auth mode "token". Valid for 30 days of
	// inactivity; store it instead of the password.
	SessionToken string           `json:"sessionToken"`
	Standings    []LadderStanding `json:"standings"`
	// The account's Discord link, nil when none.
	Discord *DiscordAccountInfo `json:"discord,omitempty"`
	// The account's email address. Present only when the server can send
	// email, so clients show the email options only then.
	Email *AccountEmailInfo `json:"email,omitempty"`
}

// LadderStanding is an account's public standing on one ladder.
// LadderStanding is an account's standing on one roster (ruleset) of a
// ladder; each roster is rated separately.
type LadderStanding struct {
	Ladder      string `json:"ladder"`
	Ruleset     string `json:"ruleset"`
	Rating      int    `json:"rating"`
	Deviation   int    `json:"deviation"`
	Tier        string `json:"tier"`
	TierIndex   int    `json:"tierIndex"`
	Provisional bool   `json:"provisional"`
	SetsPlayed  int    `json:"setsPlayed"`
}

type LadderInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	ModuleID string   `json:"moduleId"`
	Rulesets []string `json:"rulesets"`
	// Display names by ruleset id; ids without one display as themselves.
	RulesetNames map[string]string `json:"rulesetNames,omitempty"`
	BestOf       []int             `json:"bestOf"`
	RomMD5s      []string          `json:"romMd5s"`
	Tiers        []string          `json:"tiers"`
	// TeamSize is players per side: 1 (1v1) or 2 (2v2 team battles).
	TeamSize int `json:"teamSize"`
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
	// discord_unlinked: a ticket to link this Discord account with a
	// password (mode "discord_link"), and the Discord name it belongs to.
	Ticket      string `json:"ticket,omitempty"`
	DiscordName string `json:"discordName,omitempty"`
	// too_many_attempts: seconds until a login from this address is
	// accepted again.
	RetryAfter int `json:"retryAfter,omitempty"`
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
	SearchingRom    string `json:"searchingRom,omitempty"`    // ROM name while state == searching
	WatchingMatchID uint64 `json:"watchingMatchId,omitempty"` // live replay watched while state == spectating
	Verified        bool   `json:"verified,omitempty"`        // logged into a registered account
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
	Ranked       bool     `json:"ranked,omitempty"`
	Ladder       string   `json:"ladder,omitempty"` // ladder id while ranked
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
	Verified         bool `json:"verified,omitempty"`
	// Standing on the ROM's ladder, for verified players when the ROM has one.
	Standing *LadderStanding `json:"standing,omitempty"`
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
	// Present while the room is ranked.
	Ranked *RankedRoomState `json:"ranked,omitempty"`
}

type RankedRoomState struct {
	Ladder           string `json:"ladder"`
	LadderName       string `json:"ladderName"`
	ModuleID         string `json:"moduleId"`
	RulesetID        string `json:"rulesetId"`
	RulesetName      string `json:"rulesetName"`
	BestOf           int    `json:"bestOf"`
	MaxTierGap       int    `json:"maxTierGap"`
	AllowProvisional bool   `json:"allowProvisional"`
	TeamSize         int    `json:"teamSize"`
	MaxPing          int    `json:"maxPing,omitempty"`
}

// RoomPeerPingRequest reports the sender's measured round trip to another
// player in the same room.
type RoomPeerPingRequest struct {
	UserID uint64 `json:"userId"`
	RTTMs  int    `json:"rttMs"`
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
	// Seconds between the live replay broadcaster's savestate keyframes, from
	// the lobby config. Clients use 30 when it's missing (older lobbies).
	KeyframeIntervalSec int `json:"keyframeIntervalSec,omitempty"`
	// Seats is the number of controller ports every client emulates. It is
	// the room's capacity when every seated player supports late join, so an
	// empty seat is an idle controller someone can take over mid-match. Zero
	// keeps the older sizing (the highest occupied seat).
	Seats int `json:"seats,omitempty"`
	// Present for ranked matches: clients start the ranked module with these
	// settings and send MATCH_RESULT when the set ends.
	Ranked *RankedMatchInfo `json:"ranked,omitempty"`
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

type RankedMatchInfo struct {
	Ladder    string              `json:"ladder"`
	ModuleID  string              `json:"moduleId"`
	RulesetID string              `json:"rulesetId"`
	BestOf    int                 `json:"bestOf"`
	TeamSize  int                 `json:"teamSize"`
	Players   []RankedMatchPlayer `json:"players"`
}

type RankedMatchPlayer struct {
	Slot     int             `json:"slot"`
	UserID   uint64          `json:"userId"`
	Username string          `json:"username"`
	Standing *LadderStanding `json:"standing,omitempty"`
	// Team (team sets): the team this player must play on, 1 or 2 (team 1
	// holds the lowest slot), when the lobby matched teammates (a chosen or
	// kept team). 0 = the players pick teams in the game.
	Team int `json:"team,omitempty"`
}

// RankedResultPayload tells ranked players how their set was recorded.
// Status is "rated", "voided" or "disputed"; only rated sets move ratings.
type RankedResultPayload struct {
	MatchID uint64 `json:"matchId"`
	// Recorded set id (0 if recording failed); used to offer the replay.
	SetID      int64                `json:"setId,omitempty"`
	Status     string               `json:"status"`
	Reason     string               `json:"reason,omitempty"`
	WinnerSlot int                  `json:"winnerSlot,omitempty"`
	WinnerTeam int                  `json:"winnerTeam,omitempty"` // team sets
	Players    []RankedResultPlayer `json:"players"`
}

type RankedResultPlayer struct {
	Slot     int            `json:"slot"`
	Team     int            `json:"team,omitempty"`
	Username string         `json:"username"`
	Before   LadderStanding `json:"before"`
	After    LadderStanding `json:"after"`
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
	// QueueSize counts the searchers in the same pool (same ROM, and for a
	// ranked search the same ladder, roster and set length), including you.
	QueueSize int `json:"queueSize"`
	// Ranked is set when the search is (or was) a ranked one.
	Ranked bool `json:"ranked,omitempty"`
	// Continuous: the session searches again after each set or match. Stays
	// set while in a quick-match room; false once the session ends.
	Continuous bool `json:"continuous,omitempty"`
	// KeepTeammate: the session keeps its teammate after each 2v2 set.
	KeepTeammate bool `json:"keepTeammate,omitempty"`
	// Teammate: the kept teammate this search (or the next one) goes with;
	// empty when searching on your own.
	Teammate string `json:"teammate,omitempty"`
	// WaitingFor: the wanted teammate (QuickMatchJoinRequest.Teammate) who
	// hasn't searched with this player as theirs yet.
	WaitingFor string `json:"waitingFor,omitempty"`
}

// QuickMatchRematchOfferPayload: only previous opponents are available.
type QuickMatchRematchOfferPayload struct {
	Opponents []string `json:"opponents"`
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

// BroadcastJournalRequest carries the next chunk of a ranked match's write
// journal. Chunks are appended in order and never reordered or merged.
type BroadcastJournalRequest struct {
	MatchID uint64 `json:"matchId"`
	Data    string `json:"data"` // base64
}

// SpectateJournalPayload delivers journal chunk Seq (0-based, contiguous).
type SpectateJournalPayload struct {
	MatchID uint64 `json:"matchId"`
	Seq     int    `json:"seq"`
	Data    string `json:"data"` // base64
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
