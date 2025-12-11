package maxclient

// Opcode represents MAX API operation codes
type Opcode int

const (
	// System Operations
	OpPing        Opcode = 1
	OpDebug       Opcode = 2
	OpReconnect   Opcode = 3
	OpLog         Opcode = 5
	OpSessionInit Opcode = 6

	// Auth Operations
	OpProfile     Opcode = 16
	OpAuthRequest Opcode = 17
	OpAuth        Opcode = 18
	OpLogin       Opcode = 19
	OpLogout      Opcode = 20
	OpSync        Opcode = 21
	OpConfig      Opcode = 22
	OpAuthConfirm Opcode = 23

	// Contact Operations
	OpContactInfo        Opcode = 32
	OpContactAdd         Opcode = 33
	OpContactUpdate      Opcode = 34
	OpContactPresence    Opcode = 35
	OpContactList        Opcode = 36
	OpContactSearch      Opcode = 37
	OpContactInfoByPhone Opcode = 46

	// Chat Operations
	OpChatInfo          Opcode = 48
	OpChatHistory       Opcode = 49
	OpChatMark          Opcode = 50
	OpChatMedia         Opcode = 51
	OpChatDelete        Opcode = 52
	OpChatsList         Opcode = 53
	OpChatClear         Opcode = 54
	OpChatUpdate        Opcode = 55
	OpChatCheckLink     Opcode = 56
	OpChatJoin          Opcode = 57
	OpChatLeave         Opcode = 58
	OpChatMembers       Opcode = 59
	OpPublicSearch      Opcode = 60
	OpChatCreate        Opcode = 63
	OpChatMembersUpdate Opcode = 77

	// Message Operations
	OpMsgSend   Opcode = 64
	OpMsgTyping Opcode = 65
	OpMsgDelete Opcode = 66
	OpMsgEdit   Opcode = 67
	OpMsgGet    Opcode = 71
	OpMsgSearch Opcode = 73

	// File Operations
	OpPhotoUpload  Opcode = 80
	OpVideoUpload  Opcode = 82
	OpVideoPlay    Opcode = 83
	OpFileUpload   Opcode = 87
	OpFileDownload Opcode = 88
	OpLinkInfo     Opcode = 89

	// Session Operations
	OpSessionsInfo  Opcode = 96
	OpSessionsClose Opcode = 97

	// Notification Operations (server-initiated)
	OpNotifMessage             Opcode = 128
	OpNotifTyping              Opcode = 129
	OpNotifMark                Opcode = 130
	OpNotifContact             Opcode = 131
	OpNotifPresence            Opcode = 132
	OpNotifConfig              Opcode = 134
	OpNotifChat                Opcode = 135
	OpNotifAttach              Opcode = 136
	OpNotifMsgDelete           Opcode = 142
	OpNotifDraft               Opcode = 152
	OpNotifDraftDiscard        Opcode = 153
	OpNotifMsgReactionsChanged Opcode = 155
	OpNotifMsgYouReacted       Opcode = 156
	OpNotifProfile             Opcode = 159

	// Reaction Operations
	OpMsgReaction             Opcode = 178
	OpMsgCancelReaction       Opcode = 179
	OpMsgGetReactions         Opcode = 180
	OpMsgGetDetailedReactions Opcode = 181

	// Folder Operations
	OpFoldersGet     Opcode = 272
	OpFoldersGetById Opcode = 273
	OpFoldersUpdate  Opcode = 274
	OpFoldersReorder Opcode = 275
	OpFoldersDelete  Opcode = 276
	OpNotifFolders   Opcode = 277
)

// AuthType represents authentication type
type AuthType string

const (
	AuthTypeStartAuth AuthType = "START_AUTH"
	AuthTypeCheckCode AuthType = "CHECK_CODE"
	AuthTypeRegister  AuthType = "REGISTER"
)

// ChatType represents chat types
type ChatType string

const (
	ChatTypeDialog  ChatType = "DIALOG"
	ChatTypeChat    ChatType = "CHAT"
	ChatTypeChannel ChatType = "CHANNEL"
)

// MessageType represents message types
type MessageType string

const (
	MessageTypeText    MessageType = "TEXT"
	MessageTypeSystem  MessageType = "SYSTEM"
	MessageTypeService MessageType = "SERVICE"
)

// MessageStatus represents message status
type MessageStatus string

const (
	MessageStatusEdited  MessageStatus = "EDITED"
	MessageStatusRemoved MessageStatus = "REMOVED"
)

// AttachType represents attachment types
type AttachType string

const (
	AttachTypePhoto   AttachType = "PHOTO"
	AttachTypeVideo   AttachType = "VIDEO"
	AttachTypeFile    AttachType = "FILE"
	AttachTypeSticker AttachType = "STICKER"
	AttachTypeAudio   AttachType = "AUDIO"
	AttachTypeControl AttachType = "CONTROL"
)

// FormattingType represents text formatting types
type FormattingType string

const (
	FormattingStrong        FormattingType = "STRONG"
	FormattingEmphasized    FormattingType = "EMPHASIZED"
	FormattingUnderline     FormattingType = "UNDERLINE"
	FormattingStrikethrough FormattingType = "STRIKETHROUGH"
)

// DeviceType represents device types
type DeviceType string

const (
	DeviceTypeWeb     DeviceType = "WEB"
	DeviceTypeAndroid DeviceType = "ANDROID"
	DeviceTypeIOS     DeviceType = "IOS"
)

// ContactAction represents contact actions
type ContactAction string

const (
	ContactActionAdd    ContactAction = "ADD"
	ContactActionRemove ContactAction = "REMOVE"
)

// AccessType represents chat access types
type AccessType string

const (
	AccessTypePublic  AccessType = "PUBLIC"
	AccessTypePrivate AccessType = "PRIVATE"
	AccessTypeSecret  AccessType = "SECRET"
)

