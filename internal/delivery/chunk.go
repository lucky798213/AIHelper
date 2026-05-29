package delivery

import (
	"strings"
	"unicode/utf8"
)

const defaultChannelLimit = 4096

var channelLimits = map[string]int{
	"default": defaultChannelLimit,
	"feishu":  defaultChannelLimit,
}

// 把一段长文本按不同渠道的长度限制切成多段消息，尽量按段落切，避免一条消息超过平台限制
func ChunkMessage(text string, channel string) []string {
	//空文本直接返回 nil
	if text == "" {
		return nil
	}

	//CLI 通道不分片
	if strings.EqualFold(strings.TrimSpace(channel), "cli") {
		return []string{text}
	}

	//根据 channel 获取消息长度上限
	limit := channelLimit(channel)

	//如果整段文本没超限，直接返回一条
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}

	var chunks []string

	//优先按段落切分
	for _, para := range strings.Split(text, "\n\n") {

		//如果当前段落能拼到上一块里，就合并
		if len(chunks) > 0 && utf8.RuneCountInString(chunks[len(chunks)-1])+2+utf8.RuneCountInString(para) <= limit {
			chunks[len(chunks)-1] += "\n\n" + para
			continue
		}

		//如果单个段落本身太长，就硬切
		for utf8.RuneCountInString(para) > limit {
			head, tail := splitRunes(para, limit)
			chunks = append(chunks, head)
			para = tail
		}

		//剩余没超限的段落追加进去
		if para != "" {
			chunks = append(chunks, para)
		}
	}
	if len(chunks) == 0 {
		head, _ := splitRunes(text, limit)
		return []string{head}
	}
	return chunks
}

func channelLimit(channel string) int {
	name := strings.ToLower(strings.TrimSpace(channel))
	if limit, ok := channelLimits[name]; ok {
		return limit
	}
	return defaultChannelLimit
}

func splitRunes(text string, limit int) (string, string) {
	if limit <= 0 {
		return "", text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text, ""
	}
	return string(runes[:limit]), string(runes[limit:])
}
