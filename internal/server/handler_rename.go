package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/fsutil"
)

// 批量重命名：一次把若干条目的名字改掉（前端「重命名所选」）。
//
// 为什么要有独立接口，而不是让前端循环发 MOVE：**「要么全改、要么一个都别动」
// 这件事只有服务端能保证**。逐个 MOVE 时，第 5 项撞上同名文件就失败了，前 4 项
// 却已经改完、也回不去了 —— 用户面对的是一个改了一半的目录，还得自己回忆哪些
// 动过。这里分两趟：校验趟只读磁盘，任一不合格就整批拒掉并逐项说明原因；全部
// 通过才进执行趟。界面上那份「预览」因此就是最终结果。
//
// 顺带一个好处：执行趟看得见整批，于是 A→B 与 B→A 这种互相占位的改名也能做对
// （见 runRenamePlans），而逐个发请求时它们必然失败。

// maxRenameBatch 是一次允许的项数上限。
//
// 每一项都要在内存里过两趟，且校验结果要一直留到执行趟 —— 没有上限的话，一个
// 请求就能按项数吃掉内存。1000 已远超「人在界面上一屏选中」的量。
const maxRenameBatch = 1000

// renameBodyLimit 是请求体上限。
//
// 没有复用 decodeJSON 的 64 KiB：一千项的 JSON 很容易超过它，而 io.LimitReader
// 是**静默截断**的 —— 用户只会拿到一句「请求体不是合法 JSON」，根本猜不到是
// 长度问题。
const renameBodyLimit = 1 << 20

// renameItemJSON 是请求里的一项。
//
// 新名是**单段名**而不是完整目标路径（MOVE 用的是 Destination 头）：本次能力是
// 「对名字做查找替换」，本质是原地改名。收单段名让「同目录」成为接口的结构性约束
// 而不是一条运行期校验 —— 也就不可能被当成一次伪装的移动。
type renameItemJSON struct {
	// Path 是服务内 URL 路径，与列表接口下发的 entry.path 同形。
	Path string `json:"path"`
	// Name 是期望的新名（目录就是目录名），不含任何路径分隔符。
	Name string `json:"name"`
}

type renameReqJSON struct {
	Items []renameItemJSON `json:"items"`
}

type renameDoneJSON struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// renameItemErrJSON 是一项失败（或无需操作）的原因。
//
// 只有文案、没有状态码：整批被拒时状态码是 400，逐项的区别就在这一行句子里。
type renameItemErrJSON struct {
	Path   string `json:"path"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason"`
}

type renameRespJSON struct {
	OK bool `json:"ok"`
	// Message 是批次级的原因（项数为空、超上限、整批被拒、执行中出错）。
	Message string              `json:"message,omitempty"`
	Renamed []renameDoneJSON    `json:"renamed,omitempty"`
	Errors  []renameItemErrJSON `json:"errors,omitempty"`
	// Skipped 是「名称本来就没变」这类不需要动的项 —— 不是错误，但要让用户
	// 知道它为什么没出现在结果里。
	Skipped []renameItemErrJSON `json:"skipped,omitempty"`
}

// renamePlan 是校验趟算出来的「这一项该怎么改」，执行趟只认它。
type renamePlan struct {
	// idx 是输入顺序：回报与收尾都按它排，结果顺序才与用户看到的一致。
	idx    int
	path   string // 原 URL 路径（原样回报，前端据此把结果对应回预览行）
	name   string // 新名（单段）
	dirURL string // 所在目录的 URL 路径
	srcAbs string // 源绝对路径
	dstAbs string // 目标绝对路径
	// dstURL 是目标的服务内 URL 路径，由 res.Rel 算出来（多站点模式下 URL 首段
	// 是站点标签，只有 Rel 知道怎么加回去；自己拼的话前端下一次刷新列表就找不到
	// 这个路径，「保持选中」会静默失效）。
	dstURL string
	// tempAbs 只在「借道临时名」时有值（见 runRenamePlans）。
	tempAbs string
}

// handleRenameBatch 处理 POST /fs/rename-batch。
func (s *Server) handleRenameBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.cfg.AllowUpload {
		// 与单条 MOVE 同一道闸门：改名和上传一样会改动目录内容。
		http.Error(w, "403 Forbidden: 未开启重命名（--allow-upload）", http.StatusForbidden)
		return
	}

	var req renameReqJSON
	resetRead := withBodyReadDeadline(w, apiBodyReadTimeout)
	defer resetRead()
	dec := json.NewDecoder(io.LimitReader(r.Body, renameBodyLimit))
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			http.Error(w, "400 Bad Request: 请求体为空", http.StatusBadRequest)
			return
		}
		http.Error(w, "400 Bad Request: 请求体不是合法 JSON", http.StatusBadRequest)
		return
	}
	if len(req.Items) == 0 {
		s.writeJSONStatus(w, http.StatusBadRequest, renameRespJSON{Message: "没有要重命名的项"})
		return
	}
	if len(req.Items) > maxRenameBatch {
		s.writeJSONStatus(w, http.StatusBadRequest, renameRespJSON{
			Message: fmt.Sprintf("一次最多重命名 %d 项", maxRenameBatch)})
		return
	}

	// 先做「形状」检查（路径非空、新名合法、不超长）。它们不需要授权，所以放在
	// 鉴权之前 —— 一个写坏的请求体不该被回一句 401。
	if errs := s.checkRenameShapes(req.Items); len(errs) > 0 {
		s.rejectRenameBatch(w, errs)
		return
	}

	// 身份与权限：借第一项的路径过一遍 authorize。它认得 Basic、会话 cookie 与
	// 分享令牌，会按需写 401 / 403 / 429，并计入认证失败限速。
	//
	// ⚠️ 不能写成 authorize(r, "/")：一个只被授予 /docs 的账号对根目录没有权限，
	// 但它理应能改 /docs 里的名字 —— 与密钥管理刻意不用 authorize("/") 同一个
	// 理由（见 handleKeys）。逐项的真实权限在下面按各自目录再判一次。
	p, ok := s.authorize(w, r, fsutil.CleanURLPath(req.Items[0].Path))
	if !ok {
		return
	}
	if p.Perm != auth.PermReadWrite {
		http.Error(w, "403 Forbidden: 没有可写的目录权限，无法重命名", http.StatusForbidden)
		return
	}

	plans, errs := s.planRenameItems(r, req.Items)
	if len(errs) > 0 {
		s.rejectRenameBatch(w, errs)
		return
	}

	// 名称全都没变：不是错误，但也没活可干 —— 如实说，别回一个空 success
	// 让用户以为改过了。
	var skipped []renameItemErrJSON
	work := make([]renamePlan, 0, len(plans))
	for _, pl := range plans {
		if pl.srcAbs == pl.dstAbs {
			skipped = append(skipped, renameItemErrJSON{
				Path: pl.path, Name: pl.name, Reason: "名称未改变"})
			continue
		}
		work = append(work, pl)
	}
	if len(work) == 0 {
		s.writeJSONStatus(w, http.StatusBadRequest, renameRespJSON{
			Message: "没有需要改动的项（名称未改变）",
			Skipped: skipped,
		})
		return
	}

	done, fail, notes := runRenamePlans(work, s.cfg.MaxNameBytes)
	resp := renameRespJSON{Renamed: done, Skipped: skipped}
	if fail == nil {
		resp.OK = true
		s.logger.Infof("批量重命名：成功 %d 项，跳过 %d 项", len(done), len(skipped))
		s.writeJSON(w, resp)
		return
	}

	// 执行趟出错。磁盘已经被尽力还原（notes 里是还原失败的那几项，带着文件的
	// 真实位置），所以这不该被当成「改了一半」—— 但那毕竟是意外，如实回报比
	// 假装成功重要。
	reason := fail.Reason
	if len(notes) > 0 {
		resp.Message = fmt.Sprintf("执行过程中出错，有 %d 项未能还原", len(notes))
	} else {
		resp.Message = "执行过程中出错，改动已还原"
	}
	resp.Errors = append([]renameItemErrJSON{*fail}, notes...)
	s.logger.Errorf("批量重命名失败：%s / %s", fail.Path, reason)
	s.writeJSONStatus(w, http.StatusInternalServerError, resp)
}

// rejectRenameBatch 整批拒绝：一项都不动，逐项说明原因。
//
// 统一用 400，而不是 409/412/403 各来一个：调用方需要的是「一个状态码 + 一份
// 逐项原因」，拆成按条解析状态码只会让人更难写。单条 MOVE 的 412 是既有 WebDAV
// 契约，不动它。
func (s *Server) rejectRenameBatch(w http.ResponseWriter, errs []renameItemErrJSON) {
	s.writeJSONStatus(w, http.StatusBadRequest, renameRespJSON{
		Message: fmt.Sprintf("有 %d 项无法重命名，未做任何改动", len(errs)),
		Errors:  errs,
	})
}

// checkRenameShape 做与磁盘无关的形状检查，返回归一化后的路径与新名。
// reason 非空表示不合法。
//
// path 走 CleanURLPath（**与 MOVE 一致**：`..` 归约掉而不是拒绝 —— move_test.go
// 的 TestMoveDotDotStaysInside 把「清洗即安全」这个行为钉住了，新接口跟着同一条
// 规矩走）。新名则是单段名，里面的分隔符与 `.` / `..` 一律拒绝：
// 那不是「名字」，是路径。
func (s *Server) checkRenameShape(it renameItemJSON) (urlPath, name, reason string) {
	urlPath = fsutil.CleanURLPath(it.Path)
	if urlPath == "/" {
		return urlPath, "", "缺少路径"
	}
	name = it.Name
	switch {
	case name == "":
		return urlPath, name, "新名称不能为空"
	case name == "." || name == "..":
		return urlPath, name, "新名称非法"
	case strings.ContainsAny(name, `/\`):
		// 反斜杠在 Unix 上是合法字符，但它会让 Windows 客户端把名字当成两级路径。
		return urlPath, name, "新名称不能包含路径分隔符"
	}
	if _, n, tooLong := s.nameTooLong(name); tooLong {
		return urlPath, name, s.nameTooLongMsg(n)
	}
	return urlPath, name, ""
}

// checkRenameShapes 对所有项做形状检查，返回其中的不合格项。
func (s *Server) checkRenameShapes(items []renameItemJSON) []renameItemErrJSON {
	var errs []renameItemErrJSON
	for _, it := range items {
		if _, _, reason := s.checkRenameShape(it); reason != "" {
			errs = append(errs, renameItemErrJSON{Path: it.Path, Name: it.Name, Reason: reason})
		}
	}
	return errs
}

// planRenameItems 把每一项变成一份「怎么改」的计划，并做完所有**只读**校验。
//
// 返回的 errs 非空时调用方必须整批拒绝。这一步不产生任何写副作用，
// 「什么都没改」因此是天然成立的，不需要靠回滚去兜。
//
// 调用方需先过 checkRenameShapes：新名里的分隔符必须先被清掉，否则
// 「目录 + 单段名」拼出来的目标就不再是同目录的兄弟名了。
func (s *Server) planRenameItems(r *http.Request, items []renameItemJSON) ([]renamePlan, []renameItemErrJSON) {
	plans := make([]renamePlan, 0, len(items))
	// vacating 是本批次里**会离开当前位置**的源路径集合。目标若正好落在其中，
	// 说明占着它的那一项等下就会让开 —— 那是互换与链式让位，不是冲突。
	// `from == to` 的项不进来：它不会被改走，目标若是它仍然算冲突。
	vacating := make(map[string]bool, len(items))

	for i, it := range items {
		urlPath, name, _ := s.checkRenameShape(it)
		pl := renamePlan{idx: i, path: urlPath, name: name, dirURL: pathpkg.Dir(urlPath)}
		if src, err := s.res.Resolve(urlPath); err == nil {
			pl.srcAbs = src
		}
		if dst, err := s.res.Resolve(pathpkg.Join(pl.dirURL, name)); err == nil {
			pl.dstAbs = dst
			if u, rerr := s.res.Rel(dst); rerr == nil {
				pl.dstURL = u
			} else {
				pl.dstURL = pathpkg.Join(pl.dirURL, name)
			}
		}
		if pl.srcAbs != pl.dstAbs {
			vacating[pl.srcAbs] = true
		}
		plans = append(plans, pl)
	}

	reasons := make([]string, len(plans))
	s.markRenameBatchConflicts(plans, reasons)
	for i := range plans {
		if reasons[i] == "" {
			reasons[i] = s.planRenameItem(r, &plans[i], vacating)
		}
	}

	var errs []renameItemErrJSON
	for i, reason := range reasons {
		if reason != "" {
			errs = append(errs, renameItemErrJSON{
				Path: items[plans[i].idx].Path, Name: items[plans[i].idx].Name, Reason: reason})
		}
	}
	return plans, errs
}

// markRenameBatchConflicts 检查只有把整批放在一起才能发现的问题。
//
// 三件事：同一个路径出现两次、两项改成同一个新名、批次里同时有目录和它里面的条目。
// 都用「先建索引再判定」，不两两比较 —— 项数上限是 1000，O(n²) 的字符串比较
// 还能忍，但没必要。
func (s *Server) markRenameBatchConflicts(plans []renamePlan, reasons []string) {
	seenSrc := make(map[string]int, len(plans))
	srcSet := make(map[string]bool, len(plans))
	byDst := make(map[string][]int, len(plans))

	for i := range plans {
		if plans[i].srcAbs == "" {
			continue
		}
		// 同一个源出现两次：两条指令会互相打架（一个改完，另一个就找不到源了），
		// 而且它还会让下面那条「目录与子项」的判定误报成嵌套。
		if _, dup := seenSrc[plans[i].srcAbs]; dup && reasons[i] == "" {
			reasons[i] = "同一个路径在本次请求里出现了多次"
		}
		seenSrc[plans[i].srcAbs] = i
		srcSet[plans[i].srcAbs] = true

		// 两项改成同一个新名：必然有一个要覆盖另一个，两个都不放行。
		// 名称没变的项不算（它不会去占别人的位置）。
		if plans[i].dstAbs != "" && plans[i].dstAbs != plans[i].srcAbs {
			key := strings.ToLower(plans[i].dstAbs)
			byDst[key] = append(byDst[key], i)
		}
	}
	for _, group := range byDst {
		if len(group) < 2 {
			continue
		}
		for _, i := range group {
			if reasons[i] == "" {
				reasons[i] = "与本次的其它项重名（都改成了这个新名）"
			}
		}
	}

	// 同一批次里既有目录、又有它里面的条目：外层先改名，内层的源就没了，用户只会
	// 看到一句没头没脑的 ENOENT。界面上选不出这种组合（选中项都在同一层），但接口
	// 是公开的，所以在接口这一层挡掉。
	//
	// 判据是「顺着源往上走，看有没有哪一级也在本批次里」——比两两比前缀准确
	//（软链解析出来的路径照样对得上），也比两两比较快。
	for i := range plans {
		if plans[i].srcAbs == "" || reasons[i] != "" {
			continue
		}
		for dir := filepath.Dir(plans[i].srcAbs); ; dir = filepath.Dir(dir) {
			if srcSet[dir] {
				reasons[i] = "同一批次里不能同时重命名目录和它里面的条目"
				break
			}
			if parent := filepath.Dir(dir); parent == dir {
				break // 已经到根，再往上还是它自己
			}
		}
	}
}

// planRenameItem 检查单项，返回第一条不通过的原因（空串表示通过）。
//
// 顺序即报错优先级：越靠前的越「根本」，用户修起来越有方向。
func (s *Server) planRenameItem(r *http.Request, pl *renamePlan, vacating map[string]bool) string {
	switch {
	case pl.srcAbs == "":
		return "路径超出服务范围"
	case pl.dstAbs == "":
		return "目标路径超出服务范围"
	}
	// 源必须真的在磁盘上：它可能刚被别人改名或删掉（列表是缓存在页面上的）。
	if _, err := os.Lstat(pl.srcAbs); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "源已不存在（可能刚被改名或删除）"
		}
		return "无法访问源：" + err.Error()
	}
	// 站点根 / 服务根不能改名：站点条目代表的是**另一个挂载根**
	//（与 handleMove、handleDelete 同一道防线）。
	if s.res.IsRoot(pl.srcAbs) {
		return "不允许重命名服务根目录"
	}
	if pl.dstAbs != pl.srcAbs && s.res.IsRoot(pl.dstAbs) {
		return "不允许把目标设为服务根目录"
	}
	// 改的是目录里的一个条目，所以要的是**目录**的写权限。规则是目录前缀，
	// 正常情况下这与文件自身的权限一致；规则恰好只写了某个文件路径时，按文件判
	// 会放行一次注定 EACCES 的改名，按目录判才是对的。
	if s.checkPerm(r, pl.dirURL) != auth.PermReadWrite {
		return "对所在目录没有写权限"
	}
	// 源路径本身也要有写权限：规则可能给出「目录 rw + 某个文件/子目录 r」，
	// 只看目录会把只读条目改名（等于绕过只读限制）。
	if s.checkPerm(r, pl.path) != auth.PermReadWrite {
		return "对源路径没有写权限"
	}
	// 目标路径同样要有写权限：不能把条目改到一个被配置为只读的名字上。
	if s.checkPerm(r, pl.dstURL) != auth.PermReadWrite {
		return "对目标路径没有写权限"
	}
	// 软链/大小写别名：按真实目标再复核一次。
	if !s.realPermAllowed(r, pl.srcAbs, auth.PermReadWrite) {
		return "对源的真实路径没有写权限"
	}
	if !s.realPermAllowed(r, pl.dstAbs, auth.PermReadWrite) {
		return "对目标的真实路径没有写权限"
	}
	if pl.srcAbs == pl.dstAbs {
		// 名称没变：不是错误，外面按 skipped 报（放在「目标已存在」之前判，
		// 否则它会撞上自己）。
		//
		// 顺带说明为什么这里没有 handleMove 里那道「不能把目录移到它自己里面」：
		// 目标恒为「源所在目录 + 单段新名」，也就是源的**兄弟**，结构上就不可能
		// 落在源里面。收单段名而不是完整目标路径，挡掉的就是这一类。
		return ""
	}
	// 目录改名会改变所有后代的 URL：新位置的权限不能高于旧位置。
	if fi, err := os.Lstat(pl.srcAbs); err == nil && fi.IsDir() {
		if bad, ok := s.requireMoveTree(r, pl.srcAbs, pl.path, pl.dstURL); !ok {
			return "目录内的 " + bad + " 在新位置权限会提升"
		}
	}
	// 目标已存在：占着它的若是本批次里要改走的某一项，就放行。
	if _, err := os.Lstat(pl.dstAbs); err == nil {
		if !vacating[pl.dstAbs] && !sameFile(pl.srcAbs, pl.dstAbs) {
			return "目标同名已存在（未覆盖）"
		}
	}
	return ""
}

// sameFile 判断两个路径是不是同一个文件。
//
// 用于「只改大小写」的改名（a.txt → A.txt）：在 macOS / Windows 这类大小写不
// 敏感的文件系统上，os.Lstat(目标) 命中的就是源文件自己，不特判的话
// 「把扩展名改成小写」这种最常见的批量用法会被自己的存在挡成「同名已存在」。
func sameFile(a, b string) bool {
	ai, err := os.Lstat(a)
	if err != nil {
		return false
	}
	bi, err := os.Lstat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// renameStep 是执行趟的一次改名，回滚时反向再做一遍。
type renameStep struct{ from, to string }

// runRenamePlans 按依赖顺序执行改名。
//
// 顺序不是随意的：A→B 必须等 B→C 让开位置，而 A→B 与 B→A 互相占着对方的目标，
// 谁都不能先动。所以每轮只做「目标当前是空的」那些；一轮下来一个都动不了，就说明
// 剩下的都在环上 —— 那时借道一个**同目录**的临时名让开位置。
//
// 返回（已落地项, 首个失败项, 未还原的项）。任一步真实失败即停止并尽力回滚：
// POSIX 没有跨多个文件的原子提交，能做到的是把磁盘尽量还原、并把真实状态说清楚
// —— 静默留一个改了一半的目录是最坏的结果。
func runRenamePlans(plans []renamePlan, maxName int) (done []renameDoneJSON, fail *renameItemErrJSON, unreverted []renameItemErrJSON) {
	var journal []renameStep

	rollback := func() []renameItemErrJSON {
		var bad []renameItemErrJSON
		for i := len(journal) - 1; i >= 0; i-- {
			st := journal[i]
			if err := os.Rename(st.to, st.from); err != nil {
				// 还原不了的必须说出来，并给出文件此刻真实的位置 —— 否则用户会在
				// 原处找不到自己的文件，也不知道该去哪找。
				bad = append(bad, renameItemErrJSON{
					Path:   st.from,
					Reason: fmt.Sprintf("还原失败，文件现在位于 %s", st.to),
				})
			}
		}
		return bad
	}
	failWith := func(pl *renamePlan, err error) *renameItemErrJSON {
		return &renameItemErrJSON{Path: pl.path, Name: pl.name, Reason: "改名失败：" + err.Error()}
	}
	// moveToTemp 把一项挪到一个同目录的临时名上，位置就空出来了。
	moveToTemp := func(pl *renamePlan) error {
		tmp, err := allocTempName(filepath.Dir(pl.srcAbs), maxName)
		if err != nil {
			return err
		}
		if err := os.Rename(pl.srcAbs, tmp); err != nil {
			return err
		}
		journal = append(journal, renameStep{from: pl.srcAbs, to: tmp})
		pl.tempAbs = tmp
		return nil
	}

	work := make([]int, 0, len(plans))
	for i := range plans {
		work = append(work, i)
	}
	// tempUsed 是「已经借道临时名让开位置、等最后收尾」的项。
	var tempUsed []int

	for len(work) > 0 {
		next := make([]int, 0, len(work))
		for _, i := range work {
			pl := &plans[i]
			if _, err := os.Lstat(pl.dstAbs); err == nil {
				if !sameFile(pl.srcAbs, pl.dstAbs) {
					next = append(next, i) // 目标被别的项占着，等它让开
					continue
				}
				// 只改大小写：os.Rename 在大小写不敏感的文件系统上对这种改名不可靠
				//（可能静默变成 no-op），一律借道临时名两步到位。
				if err := moveToTemp(pl); err != nil {
					return done, failWith(pl, err), rollback()
				}
				tempUsed = append(tempUsed, i)
				continue
			}
			if err := os.Rename(pl.srcAbs, pl.dstAbs); err != nil {
				return done, failWith(pl, err), rollback()
			}
			journal = append(journal, renameStep{from: pl.srcAbs, to: pl.dstAbs})
			done = append(done, renameDoneJSON{From: pl.path, To: pl.dstURL})
		}
		if len(next) == len(work) {
			// 一个都没能前进：剩下的互相占着对方的目标（A→B 且 B→A）。
			// 把第一项挪到临时名上，位置就空出来了。
			pl := &plans[next[0]]
			if err := moveToTemp(pl); err != nil {
				return done, failWith(pl, err), rollback()
			}
			tempUsed = append(tempUsed, next[0])
			next = next[1:]
		}
		work = next
	}

	// 收尾：把借道的那些落到最终目标上（它们的源位置此时都已空出）。
	for _, i := range tempUsed {
		pl := &plans[i]
		if err := os.Rename(pl.tempAbs, pl.dstAbs); err != nil {
			return done, failWith(pl, err), rollback()
		}
		journal = append(journal, renameStep{from: pl.tempAbs, to: pl.dstAbs})
		done = append(done, renameDoneJSON{From: pl.path, To: pl.dstURL})
	}
	return done, nil, nil
}

// allocTempName 在 dir 下取一个还没被占用的临时名。
//
// 名字必须落在**同一个目录**：跨目录就可能跨设备，os.Rename 会退化成「复制一遍
// 再删源」，而这里要的只是让开一个位置，不该去搬数据。
//
// 不带原名（原名接近长度上限时，拼出来的临时名会超限），只带 pid 与随机后缀 ——
// 这样它天然不等于任何源或目标路径。
func allocTempName(dir string, maxLen int) (string, error) {
	for attempt := 0; attempt < 20; attempt++ {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		name := fmt.Sprintf(".gofs-rename-%d-%s", os.Getpid(), hex.EncodeToString(buf[:]))
		if maxLen > 0 && len(name) > maxLen {
			// 上限小得连临时名都放不下：换更短的写法。
			name = ".gofsrn-" + hex.EncodeToString(buf[:4])
			if len(name) > maxLen {
				return "", fmt.Errorf("名称上限 %d 过小，无法分配临时名", maxLen)
			}
		}
		p := filepath.Join(dir, name)
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			return p, nil
		}
	}
	return "", errors.New("无法分配临时文件名")
}
