package maxclient

import (
	"encoding/json"
)

// BaseMessage represents the base structure for all WebSocket messages
type BaseMessage struct {
	Ver     int             `json:"ver"`
	Cmd     int             `json:"cmd"`
	Seq     int             `json:"seq"`
	Opcode  int             `json:"opcode"`
	Payload json.RawMessage `json:"payload"`
}

// Response represents a parsed response from the server
type Response struct {
	Ver     int                    `json:"ver"`
	Cmd     int                    `json:"cmd"`
	Seq     int                    `json:"seq"`
	Opcode  int                    `json:"opcode"`
	Payload map[string]interface{} `json:"payload"`
}

// UserAgent represents user agent information for connection
type UserAgent struct {
	DeviceType   DeviceType `json:"deviceType"`
	Locale       string     `json:"locale"`
	DeviceLocale string     `json:"deviceLocale,omitempty"`
	OsVersion    string     `json:"osVersion,omitempty"`
	DeviceName   string     `json:"deviceName,omitempty"`
	AppVersion   string     `json:"appVersion"`
	Screen       string     `json:"screen,omitempty"`
	Timezone     string     `json:"timezone,omitempty"`
}

// Name represents a user's name
type Name struct {
	Name      string `json:"name,omitempty"`
	FirstName string `json:"firstName,omitempty"`
	LastName  string `json:"lastName,omitempty"`
	Type      string `json:"type,omitempty"`
}

// User represents a MAX user
type User struct {
	ID            int64    `json:"id"`
	AccountStatus int      `json:"accountStatus"`
	Names         []Name   `json:"names,omitempty"`
	Options       []string `json:"options,omitempty"`
	BaseURL       string   `json:"baseUrl,omitempty"`
	BaseRawURL    string   `json:"baseRawUrl,omitempty"`
	PhotoID       int64    `json:"photoId,omitempty"`
	Description   string   `json:"description,omitempty"`
	Gender        int      `json:"gender,omitempty"`
	Link          string   `json:"link,omitempty"`
	UpdateTime    int64    `json:"updateTime,omitempty"`
	WebApp        string   `json:"webApp,omitempty"`
}

// Me represents the current authenticated user
type Me struct {
	ID            int64    `json:"id"`
	AccountStatus int      `json:"accountStatus"`
	Phone         int64    `json:"phone"`
	Names         []Name   `json:"names"`
	UpdateTime    int64    `json:"updateTime"`
	Options       []string `json:"options,omitempty"`
}

// Contact represents a contact
type Contact struct {
	ID            int64    `json:"id"`
	AccountStatus int      `json:"accountStatus,omitempty"`
	BaseRawURL    string   `json:"baseRawUrl,omitempty"`
	BaseURL       string   `json:"baseUrl,omitempty"`
	Names         []Name   `json:"names,omitempty"`
	Options       []string `json:"options,omitempty"`
	PhotoID       int64    `json:"photoId,omitempty"`
	UpdateTime    int64    `json:"updateTime,omitempty"`
}

// Presence represents user presence information
type Presence struct {
	Seen int64 `json:"seen,omitempty"`
}

// Member represents a chat member
type Member struct {
	Contact  Contact  `json:"contact"`
	Presence Presence `json:"presence,omitempty"`
	ReadMark int64    `json:"readMark,omitempty"`
}

// Element represents a formatting element in a message
type Element struct {
	Type   FormattingType `json:"type"`
	From   int            `json:"from"`
	Length int            `json:"length"`
}

// ReactionCounter represents a reaction counter
type ReactionCounter struct {
	Reaction string `json:"reaction"`
	Count    int    `json:"count"`
}

// ReactionInfo represents reaction information on a message
type ReactionInfo struct {
	TotalCount   int               `json:"totalCount"`
	YourReaction string            `json:"yourReaction,omitempty"`
	Counters     []ReactionCounter `json:"counters,omitempty"`
}

// MessageLink represents a reply/forward link
type MessageLink struct {
	Type      string   `json:"type"`
	ChatID    int64    `json:"chatId,omitempty"`
	MessageID string   `json:"messageId,omitempty"`
	Message   *Message `json:"message,omitempty"`
}

// PhotoAttach represents a photo attachment
type PhotoAttach struct {
	Type        AttachType `json:"_type"`
	PhotoID     int64      `json:"photoId"`
	PhotoToken  string     `json:"photoToken"`
	BaseURL     string     `json:"baseUrl"`
	Width       int        `json:"width"`
	Height      int        `json:"height"`
	PreviewData string     `json:"previewData,omitempty"`
}

// VideoAttach represents a video attachment
type VideoAttach struct {
	Type        AttachType `json:"_type"`
	VideoID     int64      `json:"videoId"`
	Token       string     `json:"token"`
	Duration    int        `json:"duration"`
	Width       int        `json:"width"`
	Height      int        `json:"height"`
	Thumbnail   string     `json:"thumbnail,omitempty"`
	PreviewData string     `json:"previewData,omitempty"`
	VideoType   int        `json:"videoType,omitempty"`
}

// FileAttach represents a file attachment
type FileAttach struct {
	Type   AttachType `json:"_type"`
	FileID int64      `json:"fileId"`
	Token  string     `json:"token"`
	Name   string     `json:"name"`
	Size   int64      `json:"size"`
}

// AudioAttach represents an audio/voice attachment
type AudioAttach struct {
	Type                AttachType `json:"_type"`
	AudioID             int64      `json:"audioId"`
	URL                 string     `json:"url"`
	Duration            int        `json:"duration"`
	Wave                string     `json:"wave,omitempty"`
	Token               string     `json:"token"`
	TranscriptionStatus string     `json:"transcriptionStatus,omitempty"`
}

// ControlAttach represents a control/system attachment
type ControlAttach struct {
	Type     AttachType `json:"_type"`
	Event    string     `json:"event"`
	ChatType string     `json:"chatType,omitempty"`
	Title    string     `json:"title,omitempty"`
	UserIDs  []int64    `json:"userIds,omitempty"`
}

// Attachment represents any type of attachment
type Attachment struct {
	Type        AttachType `json:"_type"`
	PhotoID     int64      `json:"photoId,omitempty"`
	PhotoToken  string     `json:"photoToken,omitempty"`
	VideoID     int64      `json:"videoId,omitempty"`
	FileID      int64      `json:"fileId,omitempty"`
	AudioID     int64      `json:"audioId,omitempty"`
	Token       string     `json:"token,omitempty"`
	BaseURL     string     `json:"baseUrl,omitempty"`
	URL         string     `json:"url,omitempty"`
	Name        string     `json:"name,omitempty"`
	Size        int64      `json:"size,omitempty"`
	Width       int        `json:"width,omitempty"`
	Height      int        `json:"height,omitempty"`
	Duration    int        `json:"duration,omitempty"`
	PreviewData string     `json:"previewData,omitempty"`
	Event       string     `json:"event,omitempty"`
	ChatType    string     `json:"chatType,omitempty"`
	Title       string     `json:"title,omitempty"`
	UserIDs     []int64    `json:"userIds,omitempty"`
}

// Message represents a MAX message
type Message struct {
	ID           string        `json:"id"`
	ChatID       int64         `json:"chatId,omitempty"`
	Sender       int64         `json:"sender,omitempty"`
	Text         string        `json:"text"`
	Time         int64         `json:"time"`
	Type         MessageType   `json:"type"`
	Status       MessageStatus `json:"status,omitempty"`
	Options      int           `json:"options,omitempty"`
	Elements     []Element     `json:"elements,omitempty"`
	Attaches     []Attachment  `json:"attaches,omitempty"`
	Link         *MessageLink  `json:"link,omitempty"`
	ReactionInfo *ReactionInfo `json:"reactionInfo,omitempty"`
	CID          int64         `json:"cid,omitempty"`
}

// ChatOptions represents chat options/settings
type ChatOptions struct {
	OnlyOwnerCanChangeIconTitle bool `json:"ONLY_OWNER_CAN_CHANGE_ICON_TITLE,omitempty"`
	AllCanPinMessage            bool `json:"ALL_CAN_PIN_MESSAGE,omitempty"`
	OnlyAdminCanAddMember       bool `json:"ONLY_ADMIN_CAN_ADD_MEMBER,omitempty"`
	OnlyAdminCanCall            bool `json:"ONLY_ADMIN_CAN_CALL,omitempty"`
	MembersCanSeePrivateLink    bool `json:"MEMBERS_CAN_SEE_PRIVATE_LINK,omitempty"`
}

// Chat represents a MAX chat (group/channel)
type Chat struct {
	ID                       int64                  `json:"id"`
	CID                      int64                  `json:"cid,omitempty"`
	Type                     ChatType               `json:"type"`
	Title                    string                 `json:"title,omitempty"`
	Description              string                 `json:"description,omitempty"`
	Owner                    int64                  `json:"owner,omitempty"`
	Access                   AccessType             `json:"access,omitempty"`
	Link                     string                 `json:"link,omitempty"`
	Participants             map[string]int64       `json:"participants,omitempty"`
	ParticipantsCount        int                    `json:"participantsCount,omitempty"`
	Admins                   []int64                `json:"admins,omitempty"`
	AdminParticipants        map[string]interface{} `json:"adminParticipants,omitempty"`
	LastMessage              *Message               `json:"lastMessage,omitempty"`
	Options                  ChatOptions            `json:"options,omitempty"`
	Created                  int64                  `json:"created,omitempty"`
	Modified                 int64                  `json:"modified,omitempty"`
	JoinTime                 int64                  `json:"joinTime,omitempty"`
	MessagesCount            int                    `json:"messagesCount,omitempty"`
	Status                   string                 `json:"status,omitempty"`
	BaseIconURL              string                 `json:"baseIconUrl,omitempty"`
	BaseRawIconURL           string                 `json:"baseRawIconUrl,omitempty"`
	LastEventTime            int64                  `json:"lastEventTime,omitempty"`
	LastDelayedUpdateTime    int64                  `json:"lastDelayedUpdateTime,omitempty"`
	LastFireDelayedErrorTime int64                  `json:"lastFireDelayedErrorTime,omitempty"`
}

// Dialog represents a direct message conversation
type Dialog struct {
	ID                       int64            `json:"id"`
	CID                      int64            `json:"cid,omitempty"`
	Type                     ChatType         `json:"type"`
	Owner                    int64            `json:"owner"`
	Participants             map[string]int64 `json:"participants,omitempty"`
	LastMessage              *Message         `json:"lastMessage,omitempty"`
	Options                  ChatOptions      `json:"options,omitempty"`
	Created                  int64            `json:"created,omitempty"`
	Modified                 int64            `json:"modified,omitempty"`
	JoinTime                 int64            `json:"joinTime,omitempty"`
	Status                   string           `json:"status,omitempty"`
	LastEventTime            int64            `json:"lastEventTime,omitempty"`
	LastDelayedUpdateTime    int64            `json:"lastDelayedUpdateTime,omitempty"`
	LastFireDelayedErrorTime int64            `json:"lastFireDelayedErrorTime,omitempty"`
	PrevMessageID            string           `json:"prevMessageId,omitempty"`
	HasBots                  bool             `json:"hasBots,omitempty"`
}

// Session represents an active session
type Session struct {
	Client   string `json:"client"`
	Info     string `json:"info"`
	Location string `json:"location"`
	Time     int64  `json:"time"`
	Current  bool   `json:"current,omitempty"`
}

// Folder represents a chat folder
type Folder struct {
	ID         string        `json:"id"`
	Title      string        `json:"title"`
	SourceID   int64         `json:"sourceId,omitempty"`
	Include    []int64       `json:"include,omitempty"`
	Filters    []interface{} `json:"filters,omitempty"`
	Options    []interface{} `json:"options,omitempty"`
	UpdateTime int64         `json:"updateTime,omitempty"`
}

// UploadInfo represents file upload information
type UploadInfo struct {
	URL     string `json:"url"`
	FileID  int64  `json:"fileId,omitempty"`
	VideoID int64  `json:"videoId,omitempty"`
	Token   string `json:"token,omitempty"`
}

// PhotoUploadResult represents the result of a photo upload
type PhotoUploadResult struct {
	Photos map[string]struct {
		Token string `json:"token"`
	} `json:"photos"`
}

// VideoRequest represents a video download request response
type VideoRequest struct {
	URL      string `json:"url"`
	External string `json:"EXTERNAL"`
	Cache    bool   `json:"cache"`
}

// FileRequest represents a file download request response
type FileRequest struct {
	URL    string `json:"url"`
	Unsafe bool   `json:"unsafe"`
}

// Event represents a notification event from the server
type Event struct {
	Type    string                 `json:"type"`
	Opcode  Opcode                 `json:"opcode"`
	Payload map[string]interface{} `json:"payload"`
}

// SyncResponse represents the response from LOGIN/sync operation
type SyncResponse struct {
	Profile struct {
		Contact Me `json:"contact"`
	} `json:"profile"`
	Chats    []json.RawMessage `json:"chats"`
	Contacts []Contact         `json:"contacts,omitempty"`
}

// AuthResponse represents authentication response
type AuthResponse struct {
	Token      string `json:"token,omitempty"`
	TokenAttrs struct {
		Login    *TokenInfo `json:"LOGIN,omitempty"`
		Register *TokenInfo `json:"REGISTER,omitempty"`
	} `json:"tokenAttrs,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

// TokenInfo represents token information
type TokenInfo struct {
	Token string `json:"token"`
}
