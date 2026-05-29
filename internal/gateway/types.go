package gateway

type Binding struct {
	AgentID    string `yaml:"agent_id"`
	Tier       int    `yaml:"tier"`
	MatchKey   string `yaml:"match_key"`
	MatchValue string `yaml:"match_value"`
	Priority   int    `yaml:"priority"`
	DMScope    string `yaml:"dm_scope"`
}

type Route struct {
	AgentID    string // 本轮消息要交给哪个 master agent 处理
	SessionKey string // 会话存储 key，用来读取/保存这个 agent 在这个对话里的历史，格式agent:{agentID}:{channel}:direct:{peerID}
	Channel    string // 消息通道/平台，比如 cli、feishu
	PeerID     string // 对话对象 ID，比如用户 ID 或群聊 ID
}
