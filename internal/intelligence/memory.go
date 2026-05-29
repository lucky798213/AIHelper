package intelligence

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

type MemoryStore struct {
	WorkspaceDir string
	Now          func() time.Time
	Embedder     Embedder
}

type MemoryHit struct {
	Path    string
	Score   float64
	Snippet string
}

type MemorySearchResult struct {
	Hits     []MemoryHit
	Warnings []string
}

type MemoryStats struct {
	EvergreenChars int
	DailyFiles     int
	DailyEntries   int
}

type memoryEntry struct {
	Timestamp string `json:"ts"`
	Category  string `json:"category"`
	Content   string `json:"content"`
}

type memoryChunk struct {
	Path string
	Text string
}

func NewMemoryStore(workspaceDir string) MemoryStore {
	return MemoryStore{WorkspaceDir: workspaceDir, Now: time.Now}
}

func (s MemoryStore) WriteMemory(ctx context.Context, content, category string) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("memory content is required")
	}
	category = strings.TrimSpace(category)
	if category == "" {
		category = "general"
	}

	now := s.now().UTC()
	dailyDir := filepath.Join(s.WorkspaceDir, "memory", "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		return "", err
	}
	name := now.Format("2006-01-02") + ".jsonl"
	path := filepath.Join(dailyDir, name)
	entry := memoryEntry{
		Timestamp: now.Format(time.RFC3339),
		Category:  category,
		Content:   content,
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return "", err
	}
	return fmt.Sprintf("Memory saved to %s (%s)", name, category), nil
}

func (s MemoryStore) LoadEvergreen(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	raw, err := os.ReadFile(filepath.Join(s.WorkspaceDir, "MEMORY.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func (s MemoryStore) HybridSearch(ctx context.Context, query string, topK int) ([]MemoryHit, error) {
	result, err := s.HybridSearchWithWarnings(ctx, query, topK)
	if err != nil {
		return nil, err
	}
	return result.Hits, nil
}

func (s MemoryStore) HybridSearchWithWarnings(ctx context.Context, query string, topK int) (MemorySearchResult, error) {
	select {
	case <-ctx.Done():
		return MemorySearchResult{}, ctx.Err()
	default:
	}

	if topK <= 0 {
		topK = 5
	}

	//loadAllChunks会整理 memory
	//读取 MEMORY.md。
	//按空行拆成多个段落。
	//每个段落变成一个 memoryChunk{Path: "MEMORY.md", Text: para}。
	//扫描 memory/daily/*.jsonl。
	//每行 JSON 解析成 memoryEntry。
	//每条动态记忆变成一个 chunk。
	//[]memoryChunk{
	//    {Path: "MEMORY.md", Text: "Use this file for long-lived facts..."},
	//    {Path: "2026-05-21.jsonl [project]", Text: "The project uses Feishu long connection events."},
	//}
	chunks, err := s.loadAllChunks(ctx)
	if err != nil {
		return MemorySearchResult{}, err
	}
	if len(chunks) == 0 || len(tokenize(query)) == 0 {
		return MemorySearchResult{}, nil
	}

	//它会 tokenize 用户 query 和每个 memory chunk，然后算 TF-IDF cosine 相似度。
	keywordResults := s.keywordSearch(query, chunks, 10)

	//配置了 embedding，就用外部 embedding；如果没配置，就用本地 hashVector 做一个轻量向量近似。
	vectorResults, warnings := s.vectorSearch(ctx, query, chunks, 10)

	//把 vector 结果和 keyword 结果合并，7：3
	merged := mergeHybridResults(vectorResults, keywordResults, 0.7, 0.3)

	//如果 path 里有日期，比如 2026-05-21.jsonl，越旧分数越低。MEMORY.md 没日期，所以不衰减。
	decayed := s.temporalDecay(merged, 0.01)

	//去重和多样性重排。它避免 topK 全是高度相似的内容。
	reranked := mmrRerank(decayed, 0.7)

	limit := topK
	if len(reranked) < limit {
		limit = len(reranked)
	}
	hits := make([]MemoryHit, 0, limit)
	for _, result := range reranked[:limit] {
		hits = append(hits, MemoryHit{
			Path:    result.Chunk.Path,                      //保留记忆的存储路径 Path
			Score:   math.Round(result.Score*10000) / 10000, //把分数四舍五入保留 4 位小数（让分数更整洁，不显示冗长的小数）
			Snippet: snippet(result.Chunk.Text, 200),        //截取记忆文本的前 200 个字符作为摘要 Snippet
		})
	}
	return MemorySearchResult{Hits: hits, Warnings: warnings}, nil
}

func (s MemoryStore) loadAllChunks(ctx context.Context) ([]memoryChunk, error) {
	var chunks []memoryChunk

	evergreen, err := s.LoadEvergreen(ctx)
	if err != nil {
		return nil, err
	}
	for _, para := range strings.Split(evergreen, "\n\n") {
		para = strings.TrimSpace(para)
		if para != "" {
			chunks = append(chunks, memoryChunk{Path: "MEMORY.md", Text: para})
		}
	}

	dailyDir := filepath.Join(s.WorkspaceDir, "memory", "daily")
	entries, err := os.ReadDir(dailyDir)
	if err != nil {
		if os.IsNotExist(err) {
			return chunks, nil
		}
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dailyDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var item memoryEntry
			if err := json.Unmarshal([]byte(line), &item); err != nil {
				continue
			}
			text := strings.TrimSpace(item.Content)
			if text == "" {
				continue
			}
			label := entry.Name()
			if strings.TrimSpace(item.Category) != "" {
				label += " [" + strings.TrimSpace(item.Category) + "]"
			}
			chunks = append(chunks, memoryChunk{Path: label, Text: text})
		}
	}
	return chunks, nil
}

type scoredMemory struct {
	Chunk memoryChunk
	Score float64
}

func (s MemoryStore) keywordSearch(query string, chunks []memoryChunk, topK int) []scoredMemory {

	// 把查询词拆成一个个关键词（分词）
	queryTokens := tokenize(query)
	if len(queryTokens) == 0 {
		return nil
	}

	// 存储：每一条记忆 分词后的结果
	chunkTokens := make([][]string, len(chunks))

	// df = document frequency（文档频率）
	// 作用：统计「每个关键词，在多少条记忆里出现过」
	// 例："的" 在100条记忆里都有 → df["的"]=100
	df := map[string]int{}

	// 遍历每一条记忆
	for i, chunk := range chunks {
		// 1. 给当前这条记忆分词
		tokens := tokenize(chunk.Text)

		// 存起来，后面要用
		chunkTokens[i] = tokens

		// 这里是找出这段 tokens 出现了什么词，出现的词就放到 seen 中，然后变量 seen，在 df 中给对应的词++，表示拥有这个词的记忆数+1
		seen := map[string]struct{}{}
		for _, token := range tokens {
			seen[token] = struct{}{}
		}

		// 3. 更新df：这个词又多出现了1条记忆
		//记录的是包含该词的文档数
		for token := range seen {
			df[token]++
		}
	}

	// n = 总共有多少条记忆
	n := len(chunks)

	//计算「查询词的 TF-IDF 向量」
	// 计算查询词的 TF-IDF 向量
	// TF-IDF：给每个词算「重要性权重」
	// 规则：越稀有的词，权重越高；越常见的词，权重越低
	qVec := tfidf(queryTokens, df, n)

	//计算「每条记忆和查询词的相似度」
	scored := make([]scoredMemory, 0)

	// 存储：带分数的记忆（分数越高，越相关）
	// 遍历每一条记忆的分词结果
	for i, tokens := range chunkTokens {
		// 空记忆跳过
		if len(tokens) == 0 {
			continue
		}

		// 1. 计算这条记忆的 TF-IDF 向量
		// 2. 用「余弦相似度」计算：查询词向量 和 记忆向量 的相似程度
		score := cosine(qVec, tfidf(tokens, df, n))

		// 只保留「有相关性」的结果（分数>0）
		if score > 0 {
			scored = append(scored, scoredMemory{Chunk: chunks[i], Score: score})
		}
	}

	//排序 + 取 TopK 条最相关的结果
	// 按分数「从高到低」排序
	sortScored(scored)

	// 只返回前 topK 条最相关的
	return topScored(scored, topK)
}

// 方法：向量搜索（语义搜索）
// 入参：
//
//	ctx: 上下文（控制超时/取消）
//	query: 用户的问题/查询词
//	chunks: 所有记忆片段
//	topK: 返回最相关的前K条记忆
//
// 返回值：
//  1. 带相似度分数的记忆列表
//  2. 警告信息（AI模型报错时记录）
func (s MemoryStore) vectorSearch(ctx context.Context, query string, chunks []memoryChunk, topK int) ([]scoredMemory, []string) {
	if s.Embedder == nil {
		return hashVectorSearch(query, chunks, topK), nil
	}

	scored, err := s.embeddingVectorSearch(ctx, query, chunks, topK)
	if err != nil {
		warning := fmt.Sprintf("embedding failed; fell back to hash-vector for all vector comparisons: %v", err)
		return hashVectorSearch(query, chunks, topK), []string{warning}
	}
	return scored, nil
}

func hashVectorSearch(query string, chunks []memoryChunk, topK int) []scoredMemory {
	qVec := hashVector(query, 64)
	scored := make([]scoredMemory, 0, len(chunks))
	for _, chunk := range chunks {
		cVec := hashVector(chunk.Text, 64)
		score := vectorCosine(qVec, cVec)
		if score > 0 {
			scored = append(scored, scoredMemory{Chunk: chunk, Score: score})
		}
	}
	sortScored(scored)
	return topScored(scored, topK)
}

func (s MemoryStore) embeddingVectorSearch(ctx context.Context, query string, chunks []memoryChunk, topK int) ([]scoredMemory, error) {
	qVec, err := s.Embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query embedding failed: %w", err)
	}

	chunkVectors := make([][]float64, len(chunks))
	for i, chunk := range chunks {
		vector, err := s.Embedder.Embed(ctx, chunk.Text)
		if err != nil {
			return nil, fmt.Errorf("chunk embedding failed for %s: %w", chunk.Path, err)
		}
		chunkVectors[i] = vector
	}

	scored := make([]scoredMemory, 0, len(chunks))
	for i, chunk := range chunks {
		score := vectorCosine(qVec, chunkVectors[i])
		if score > 0 {
			scored = append(scored, scoredMemory{Chunk: chunk, Score: score})
		}
	}
	sortScored(scored)
	return topScored(scored, topK), nil
}

// vectorResults: 向量/语义搜索 的结果（懂意思，认语义）
// keywordResults: TF-IDF关键词搜索 的结果（认字，精准匹配）
// vectorWeight: 语义结果的权重（比如 0.7，代表70%重要性）
// textWeight: 关键词结果的权重（比如 0.3，代表30%重要性）
func mergeHybridResults(vectorResults, keywordResults []scoredMemory, vectorWeight, textWeight float64) []scoredMemory {
	// 用map存储：key=记忆文本片段（唯一标识），value=加权后的记忆结果
	// 作用：自动去重！同一条记忆只会存一次
	merged := map[string]scoredMemory{}

	// 遍历所有语义搜索的结果
	for _, result := range vectorResults {
		// 生成唯一标识key，截取记忆文本的前 100 个字符作为唯一 key
		key := snippet(result.Chunk.Text, 100)

		// 分数 × 语义权重（调整重要性）
		result.Score *= vectorWeight

		// 存入map
		merged[key] = result
	}

	// 遍历所有关键词搜索的结果

	for _, result := range keywordResults {
		key := snippet(result.Chunk.Text, 100)

		// 分数 × 关键词权重
		result.Score *= textWeight

		// 如果这条记忆，**语义搜索里已经有了**（重复）
		if existing, ok := merged[key]; ok {
			// 分数叠加！同时被两种搜索命中 → 分数更高
			existing.Score += result.Score
			merged[key] = existing
			continue
		}
		// 如果没有，直接存入map
		merged[key] = result
	}

	// 把map转成切片
	results := make([]scoredMemory, 0, len(merged))
	for _, result := range merged {
		results = append(results, result)
	}

	// 按总分从高到低排序
	sortScored(results)

	// 返回最终融合后的结果
	return results
}

func (s MemoryStore) temporalDecay(results []scoredMemory, decayRate float64) []scoredMemory {
	//从记忆的文件路径（Path）里提取 YYYY-MM-DD 格式的日期；获取当前的 UTC 时间。
	dateRe := regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	now := s.now().UTC()

	//遍历所有记忆结果
	for i := range results {
		match := dateRe.FindString(results[i].Chunk.Path)
		if match == "" {
			continue
		}
		date, err := time.Parse("2006-01-02", match)
		if err != nil {
			continue
		}

		//用当前时间减去记忆的创建时间，算出这条记忆过去了多少天
		ageDays := now.Sub(date).Hours() / 24
		if ageDays > 0 {
			//新 score = 旧 score * e 的-（decayRate*ageDays）次方
			results[i].Score *= math.Exp(-decayRate * ageDays)
		}
	}
	sortScored(results)
	return results
}

func mmrRerank(results []scoredMemory, lambda float64) []scoredMemory {
	// 边界条件：结果≤1条，不用重排直接返回
	if len(results) <= 1 {
		return results
	}

	// 1. 对所有记忆文本分词（用你之前的tokenize函数）
	// 为了后续计算文本相似度
	tokenized := make([][]string, len(results))
	for i, result := range results {
		tokenized[i] = tokenize(result.Chunk.Text)
	}

	// 2. 初始化三个核心列表
	selected := make([]int, 0, len(results)) // 已经选中的记忆索引
	remaining := make([]int, len(results))   // 还没选的记忆索引
	for i := range results {
		remaining[i] = i
	}
	reranked := make([]scoredMemory, 0, len(results)) // 最终重排后的结果

	// 3. 核心循环：一条一条挑选最优的记忆
	for len(remaining) > 0 {
		bestPos := 0            // 最优结果的位置
		bestMMR := math.Inf(-1) // 最优MMR分数（初始负无穷）

		// 遍历所有「剩余未选」的记忆
		for pos, idx := range remaining {
			maxSimilarity := 0.0
			// 计算：当前记忆 和 「所有已选记忆」的最大相似度
			// （如果和已选的很像，就会被扣分）
			for _, selectedIdx := range selected {
				if sim := jaccard(tokenized[idx], tokenized[selectedIdx]); sim > maxSimilarity {
					maxSimilarity = sim
				}
			}

			// ✅ MMR 核心计算公式
			// 分数 = 相关性权重 - 重复性惩罚
			mmr := lambda*results[idx].Score - (1-lambda)*maxSimilarity

			// 记录分数最高的那条
			if mmr > bestMMR {
				bestMMR = mmr
				bestPos = pos
			}
		}

		// 4. 选中最优的那条
		chosen := remaining[bestPos]
		selected = append(selected, chosen)          // 加入已选列表
		reranked = append(reranked, results[chosen]) // 加入最终结果

		// 从剩余列表中移除选中的项
		remaining = append(remaining[:bestPos], remaining[bestPos+1:]...)
	}

	// 返回重排后：既相关、又不重复的最终结果
	return reranked
}

func FormatMemoryHits(hits []MemoryHit) string {
	if len(hits) == 0 {
		return "No relevant memories found."
	}
	lines := make([]string, 0, len(hits))
	for _, hit := range hits {
		lines = append(lines, fmt.Sprintf("[%s] (score: %.4f) %s", hit.Path, hit.Score, hit.Snippet))
	}
	return strings.Join(lines, "\n")
}

func FormatMemorySearchResult(result MemorySearchResult) string {
	parts := make([]string, 0, len(result.Warnings)+1)
	for _, warning := range result.Warnings {
		parts = append(parts, "[warning] "+warning)
	}
	parts = append(parts, FormatMemoryHits(result.Hits))
	return strings.Join(parts, "\n")
}

func (s MemoryStore) Stats(ctx context.Context) (MemoryStats, error) {
	evergreen, err := s.LoadEvergreen(ctx)
	if err != nil {
		return MemoryStats{}, err
	}
	stats := MemoryStats{EvergreenChars: len([]rune(evergreen))}
	dailyDir := filepath.Join(s.WorkspaceDir, "memory", "daily")
	entries, err := os.ReadDir(dailyDir)
	if err != nil {
		if os.IsNotExist(err) {
			return stats, nil
		}
		return MemoryStats{}, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		stats.DailyFiles++
		raw, err := os.ReadFile(filepath.Join(dailyDir, entry.Name()))
		if err != nil {
			return MemoryStats{}, err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) != "" {
				stats.DailyEntries++
			}
		}
	}
	return stats, nil
}

// 给一段文本（查询 / 记忆）里的每个词，计算一个「重要性权重」
func tfidf(tokens []string, df map[string]int, n int) map[string]float64 {
	//记录 tokens 这句话的词频（TF）
	tf := map[string]int{}
	for _, token := range tokens {
		tf[token]++
	}

	// vec = 最终的权重向量（词 → 权重）
	vec := map[string]float64{}

	// 遍历每个词的 TF 值，
	for token, count := range tf {
		//计算这个词的权重
		vec[token] = float64(count) * (math.Log(float64(n+1)/float64(df[token]+1)) + 1)
	}
	return vec
}

// 用余弦相似度公式，计算两个向量的相似分数
func cosine(a, b map[string]float64) float64 {
	dot := 0.0
	//计算「点积」：两个向量的匹配程度
	for key, av := range a {
		dot += av * b[key]
	}

	// 2. 计算两个向量各自的模长
	na := norm(a)
	nb := norm(b)

	// 3. 防止分母为0（无意义，直接返回0）
	if na == 0 || nb == 0 {
		return 0
	}

	// 4. 余弦相似度公式：点积 / (向量a长度 × 向量b长度)
	return dot / (na * nb)
}

// 计算一个 TF-IDF 向量的长度（模长）
func norm(vec map[string]float64) float64 {
	sum := 0.0

	// 遍历所有词的权重，把 权重×权重 加起来
	for _, value := range vec {
		sum += value * value
	}

	// 开平方，就是向量的模长
	return math.Sqrt(sum)
}

func hashVector(text string, dim int) []float64 {
	vec := make([]float64, dim)
	for _, token := range tokenize(text) {
		hash := stableHash(token)
		for i := 0; i < dim; i++ {
			if (hash>>uint(i%62))&1 == 1 {
				vec[i] += 1
			} else {
				vec[i] -= 1
			}
		}
	}
	length := 0.0
	for _, value := range vec {
		length += value * value
	}
	length = math.Sqrt(length)
	if length == 0 {
		return vec
	}
	for i := range vec {
		vec[i] /= length
	}
	return vec
}

func stableHash(value string) uint64 {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(value))
	return hasher.Sum64()
}

func vectorCosine(a, b []float64) float64 {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	if limit == 0 {
		return 0
	}
	dot := 0.0
	na := 0.0
	nb := 0.0
	for i := 0; i < limit; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// 切分 text，变成一个个字
func tokenize(text string) []string {
	// 1. 定义最终要返回的 关键词列表
	var tokens []string

	// 2. 临时容器：用来拼接 英文/数字 单词
	var current strings.Builder

	// 3. 内部小函数：刷新临时容器（把拼好的词存到 tokens 中，清空容器）
	flush := func() {
		token := current.String()
		current.Reset()

		// 核心规则：只有 长度>1 的词，才保留（过滤单字符）
		if len([]rune(token)) > 1 {
			tokens = append(tokens, token)
		}
	}

	//r 类型是 rune
	// 4. 遍历文本的每一个字符（用rune，完美支持中文）
	for _, r := range text {
		// ===== 情况1：字符是 中文汉字 =====
		if unicode.Is(unicode.Han, r) {
			flush()
			tokens = append(tokens, string(r)) // 中文单字，直接作为一个词
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(unicode.ToLower(r)) // 转小写，拼进临时容器
			continue
		}
		// ===== 情况3：字符是 标点/空格/符号（无效字符） =====
		flush() // 分割单词，刷新容器
	}
	flush()
	// 6. 返回最终的关键词列表
	return tokens
}

func jaccard(a, b []string) float64 {
	setA := map[string]struct{}{}
	setB := map[string]struct{}{}
	for _, token := range a {
		setA[token] = struct{}{}
	}
	for _, token := range b {
		setB[token] = struct{}{}
	}
	if len(setA) == 0 && len(setB) == 0 {
		return 0
	}
	intersection := 0
	for token := range setA {
		if _, ok := setB[token]; ok {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func sortScored(scored []scoredMemory) {
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})
}

func topScored(scored []scoredMemory, topK int) []scoredMemory {
	if topK <= 0 || len(scored) <= topK {
		return scored
	}
	return scored[:topK]
}

func snippet(text string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "..."
}

func (s MemoryStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
