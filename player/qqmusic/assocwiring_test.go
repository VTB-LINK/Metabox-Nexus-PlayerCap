package qqmusic

// 本文件守 runSession 接入歌词关联（#218，设计 §6.8）的接线形状。runSession 要真进程、没有离线夹具（sessionreplay_test.go
// 头注），歌词状态机的逻辑由 lyricstate*_test.go 覆盖，取结果的 takeResult 由 assoctakeresult_test.go 覆盖；这里以源码扫描
// 查接线，只查形状，不替代读代码。末尾的用例以改写过的源码自证扫描拦得住各处变异。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

// recordReaders 是直接读 QQ 内存里当前歌曲记录的函数（songrecord.go 与读取者，设计 §6.4 第 4 步）：只在读取者 goroutine 里
// 调用，runSession 全函数都不得直接调用（会话只经 newRecordSource 复制只读句柄交给读取者）。
var recordReaders = []string{"scanRecord", "readSongPrefix", "readNarrowField", "procMemSource", "scanCurrentRecord", "readRecord",
	"VirtualQueryEx", "ReadProcessMemory"}

// fillQueries 是读取者补全编号的兜底查询（设计 §6.4 第 2 步「兜底」）：只在读取者 goroutine 里调用，runSession 全函数都不得直接
// 调用（会话只交出封面请求带回的详情结果）。
var fillQueries = []string{"fetchSongMidByID", "fetchSongIDByMid"}

// assocForbidden 是轮询线程上取结果、换词的路径里不得调用的函数：联网、读文件、扫内存、读当前歌曲记录、编号兜底查询与 Restart
// Manager 查询（设计 §6.10）。
var assocForbidden = append(append([]string{"fetchLRC", "fetchCoverURL", "readShared", "FindSongMid", "FindCookie",
	"ReadAllMetadata", "Holders", "FetchCoverBase64"}, recordReaders...), fillQueries...)

// assocWiringProblems 检查 src（qqmusic.go 的源码）里歌词关联的接线，返回发现的问题：
//   - 会话开始以可取消的 context 启动读取者，defer cancel() 紧跟在建 context 之后、启动之前，所有退出路径都取消；
//   - runSession 里没有阻塞的接收：每个 select 都带 default，<- 只出现在 select 的 case 上；结果通道只经 takeResult 取，
//     takeResult 是一个带 default 的 select，不循环；
//   - 换歌分支：直属语句依次为 prevLocal := lyricSt.localPath()（上一首采纳的本地文件）、以本拍的 loadUsable 求改名证据
//     renamed := loadOpen.claim(loadUsable, mem.Offsets().PlayState, pollInterval)、logRenameEvidence(prevLocal, renamed)、
//     assocKey = songKey{…}（gen 加一，excludeLocal 只经 excludeFor(prevLocal, renamed) 交出）、lyricSt.reset(assocKey.gen)，
//     loadOpen.claim 与 lyricSt.localPath 在 runSession 里各恰一处，excludeLocal 在 runSession 里只出现在这个字面量里（补取块与
//     重开不碰它）；Submit 在 fetchLRC 之前；fetchLRC、fetchCoverURL 之后在 err == nil
//     时 setOnlineRaw、refresh、takeResult 各一次；编号缺位的一支以 refresh 登记「无」已给出；发歌词之前置
//     isPaused = false 与 lastLineIdx = -1（换歌时不在暂停中，不必再冻结）；
//   - 改名证据的窗口与载入段的记录（#218 第 5 版，设计 §6.8「改名证据」）：loadOpen 只由 runSession 函数体直属的
//     loadOpen := newLoadEdge(pollInterval) 建出一次，在 pollInterval := EffectivePollInterval(p.pollMs) 之后、轮询循环之前，
//     别处不给它赋值，没有 loadEdge{…} 复合字面量与类型为 loadEdge 的 var；轮询循环体里紧跟 meta, err := mem.ReadAllMetadata()
//     的 if err != nil 恰为 loadOpen.missed() 与 continue 两句（这一拍没有读到播放状态），loadOpen.missed 全函数恰一处；三处
//     loadOpen.step 都传 sampleTime（该次读数的时刻，见下面「同一首重开」）；
//   - 补取块：编号由缺到有时 gen 加一、reset、Submit，都在 fetchLRC 之前；取词成功后 setOnlineRaw、refresh、takeResult，
//     无条件经 showLyrics 发出，显示的那份不变时以 currentLyrics 重发；lyricsPending 的赋值与 main 的补取块相同（#227：
//     case backfillGiveUp 与取词成功处各一个 false）；放弃时的超时日志在来源为本地文件时走 log.Detail；
//   - 详情结果的交接（设计 §6.4 第 2 步「交接」）：fetchCoverURL 恰两处、位置同前（换歌分支在 fetchLRC 之后，补取块在取词成功
//     经 showLyrics 发出之后），各紧跟 if k, ok := applyDetail(assocKey, 所查编号, 详情结果); ok { assocKey = k;
//     reader.Submit(assocKey) }，所查编号分别为 meta.SongID, meta.SongMid 与 sid, smid；补取块的那一处之后紧跟以
//     exhaustDetail(assocKey) 为条件的同形 if（所查编号衔接不上时不滞留尚无）；补取的 case backfillGiveUp 与 case backfillHold
//     各有同样的 if；补取块编号由缺到有时 assocKey.detail 清为尚无；reader.Submit 恰 8 处（换歌分支、补取块编号由缺到有、
//     reopenSong，加这五处）；runSession 全函数不调用 fillQueries；
//   - 每拍：暂停检测（if v.pause，#172）之后、行匹配之前，已认领时经 takeResult 取一条、有变化即以锚点经 showLyrics 发出；
//   - showLyrics：先以 lyricsUndecided 判没有结论，显示本就为空即返回；再 lastLineIdx = -1，位置取
//     LivePosition(…, !isPaused, …)，行为空且有结论走 emitNoLyrics，否则 all_lyrics；之后已判暂停时在同一位置 pos 补发
//     playback_pause，入队与否记进 repausePending；
//   - 补发暂停：emitFrozenPause 用 TryEmit；入队失败时暂停检测之后每拍按 repausePos 再试；每处 isPaused = false 之后紧跟
//     repausePending = false；repausePending、repausePos 另只在暂停中跳转的补发处赋值（#222）；
//   - 发 all_lyrics 的只有换歌分支（三处）与 showLyrics（两处），showLyrics 只在补取块与每拍各调一次；
//   - takeResult、showLyrics 与每拍的那一块不调用 assocForbidden 里的函数，runSession 全函数不调用 recordReaders 里的函数；
//   - 同一首重开（设计 §6.8「重开」）：reopen 加一只在 reopenSong 闭包里，闭包依次 assocKey.reopen++、
//     lyricSt.reopened(assocKey.reopen)、reader.Submit(assocKey)，不 reset、不碰 assocKey.gen；reopenSong 恰四处调用，条件都
//     出自同一个 loadUsable（在循环体里恰赋值一次、出自 loadingUsable(judge.bad, pollInterval)），载入支与暂扣支互斥，每拍
//     至多写入一次（循环体里的 reopenedThisTick := false，只在四处写入时置真）：拍首载入支在 sampleTime 之后、换歌检测与
//     hold.step 之前，以首读 loadOpen.step（传 sampleTime）、经 reopenByLoading 判定；换歌分支与补取块取词之后的重读被采用、
//     换上播放状态之后，以重读 step（传重读之后的 sampleTime）、按同样的条件写入（两处写法相同）；暂扣支在变化分支的 if seekDetected 里，条件为
//     !loadUsable、released 且读数低于暂扣前、编号已知；loadEdge.step 与 judgeTick 都经 classify 分类；reopenByLoading 的
//     条件为可用、由无到有、已认领、歌名相等、编号已知；换歌分支新建的 songKey 不带 reopen，补取块 gen 加一之后、Submit 之前
//     assocKey.reopen = 0。
func assocWiringProblems(src string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "qqmusic.go", src, 0)
	if err != nil {
		return []string{"解析失败: " + err.Error()}
	}
	var run, take, frozen, loadStep, judgeFn, byLoading *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			switch {
			case fd.Recv != nil && fd.Name.Name == "runSession":
				run = fd
			case fd.Recv != nil && fd.Name.Name == "emitFrozenPause":
				frozen = fd
			case fd.Recv != nil && fd.Name.Name == "step" && types.ExprString(fd.Recv.List[0].Type) == "*loadEdge":
				loadStep = fd
			case fd.Recv == nil && fd.Name.Name == "takeResult":
				take = fd
			case fd.Recv == nil && fd.Name.Name == "judgeTick":
				judgeFn = fd
			case fd.Recv == nil && fd.Name.Name == "reopenByLoading":
				byLoading = fd
			}
		}
	}
	if run == nil || take == nil {
		return []string{"没找到 runSession 或 takeResult —— 门禁失效"}
	}

	var probs []string
	bad := func(format string, a ...any) { probs = append(probs, fmt.Sprintf(format, a...)) }
	str := types.ExprString
	// named 返回 n 里（含函数字面量）调用 name 的调用，按源码次序。name 带点（如 lyricSt.reset）时连接收者一起比，
	// 否则只比名字、不论接收者。
	named := func(n ast.Node, name string) []*ast.CallExpr {
		var out []*ast.CallExpr
		ast.Inspect(n, func(x ast.Node) bool {
			if c, ok := x.(*ast.CallExpr); ok {
				switch fun := c.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == name || str(fun) == name {
						out = append(out, c)
					}
				case *ast.Ident:
					if fun.Name == name {
						out = append(out, c)
					}
				}
			}
			return true
		})
		return out
	}
	first := func(n ast.Node, name string) token.Pos {
		if cs := named(n, name); len(cs) > 0 {
			return cs[0].Pos()
		}
		return token.NoPos
	}
	// ordered 报告 n 里各调用的首次出现都存在且按 names 的次序。
	ordered := func(n ast.Node, names ...string) bool {
		prev := token.NoPos
		for _, name := range names {
			p := first(n, name)
			if p == token.NoPos || p < prev {
				return false
			}
			prev = p
		}
		return true
	}
	// assigns 返回 n 里形如 lhs = rhs（单值，= 或 :=）的语句。
	assigns := func(n ast.Node, lhs, rhs string) []*ast.AssignStmt {
		var out []*ast.AssignStmt
		ast.Inspect(n, func(x ast.Node) bool {
			if a, ok := x.(*ast.AssignStmt); ok && len(a.Lhs) == 1 && len(a.Rhs) == 1 && str(a.Lhs[0]) == lhs &&
				(rhs == "" || str(a.Rhs[0]) == rhs) {
				out = append(out, a)
			}
			return true
		})
		return out
	}
	// field 返回 e（T{…} 或 &T{…}）里键为 key 的值；没有为空。
	field := func(e ast.Expr, key string) string {
		if u, ok := e.(*ast.UnaryExpr); ok {
			e = u.X
		}
		if cl, ok := e.(*ast.CompositeLit); ok {
			for _, el := range cl.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok && str(kv.Key) == key {
					return str(kv.Value)
				}
			}
		}
		return ""
	}
	// stmt 把单个表达式语句、自增自减或单值赋值写成源码形式；其余为空。
	stmt := func(s ast.Stmt) string {
		switch s := s.(type) {
		case *ast.ExprStmt:
			return str(s.X)
		case *ast.IncDecStmt:
			return str(s.X) + s.Tok.String()
		case *ast.AssignStmt:
			if len(s.Lhs) == 1 && len(s.Rhs) == 1 {
				return str(s.Lhs[0]) + " " + s.Tok.String() + " " + str(s.Rhs[0])
			}
		}
		return ""
	}
	// ifWithCond 返回 n 里条件为 cond 的第一个 if。
	ifWithCond := func(n ast.Node, cond string) *ast.IfStmt {
		var out *ast.IfStmt
		ast.Inspect(n, func(x ast.Node) bool {
			if s, ok := x.(*ast.IfStmt); ok && out == nil && str(s.Cond) == cond {
				out = s
			}
			return out == nil
		})
		return out
	}
	// lyricEmits 返回 n 里发歌词的调用：emitNoLyrics 与 Emit(player.EventAllLyrics, …)。
	lyricEmits := func(n ast.Node) []*ast.CallExpr {
		out := named(n, "emitNoLyrics")
		for _, c := range named(n, "Emit") {
			if len(c.Args) > 0 && str(c.Args[0]) == "player.EventAllLyrics" {
				out = append(out, c)
			}
		}
		return out
	}
	noForbidden := func(where string, n ast.Node) {
		for _, name := range assocForbidden {
			if len(named(n, name)) > 0 {
				bad("%s里调用了 %s（轮询线程上取结果、换词不得联网、读文件或查 Restart Manager）", where, name)
			}
		}
	}

	// ---- 会话开始：启动读取者与取消 ----
	body := run.Body.List
	loopIdx, ctxIdx, showIdx := -1, -1, -1
	var show *ast.FuncLit
	for i, s := range body {
		switch s := s.(type) {
		case *ast.ForStmt:
			if s.Cond == nil {
				loopIdx = i
			}
		case *ast.AssignStmt:
			if len(s.Rhs) == 1 && len(s.Lhs) == 2 && str(s.Lhs[0]) == "ctx" && str(s.Lhs[1]) == "cancel" &&
				strings.HasPrefix(str(s.Rhs[0]), "context.WithCancel(") {
				ctxIdx = i
			}
			if fl, ok := s.Rhs[0].(*ast.FuncLit); ok && len(s.Lhs) == 1 && str(s.Lhs[0]) == "showLyrics" {
				showIdx, show = i, fl
			}
		}
	}
	if loopIdx < 0 || show == nil {
		return append(probs, "没找到 runSession 的轮询循环或 showLyrics —— 门禁失效")
	}
	loop := body[loopIdx].(*ast.ForStmt)
	switch {
	case ctxIdx < 0 || ctxIdx+2 >= loopIdx:
		bad("会话开始应先 ctx, cancel := context.WithCancel(…)，再 defer cancel()、启动读取者，都在轮询循环之前")
	default:
		if d, ok := body[ctxIdx+1].(*ast.DeferStmt); !ok || str(d.Call) != "cancel()" {
			bad("defer cancel() 应紧跟在建 context 之后")
		}
		if a, ok := body[ctxIdx+2].(*ast.AssignStmt); !ok || len(a.Rhs) != 1 ||
			str(a.Rhs[0]) != "startAssocReader(ctx, defaultAssocDeps(mem.pid, mem.Version(), newRecordSource(mem)))" {
			bad("defer cancel() 之后应紧跟 startAssocReader(ctx, defaultAssocDeps(mem.pid, mem.Version(), newRecordSource(mem)))")
		}
	}
	if n := len(named(f, "startAssocReader")); n != 1 {
		bad("qqmusic.go 里 startAssocReader 应恰调一次，实得 %d", n)
	}

	// ---- runSession 里没有阻塞的接收 ----
	caseRecv := map[*ast.UnaryExpr]bool{}
	ast.Inspect(run, func(x ast.Node) bool {
		s, ok := x.(*ast.SelectStmt)
		if !ok {
			return true
		}
		hasDefault := false
		for _, c := range s.Body.List {
			cc := c.(*ast.CommClause)
			switch comm := cc.Comm.(type) {
			case nil:
				hasDefault = true
			case *ast.ExprStmt:
				if u, ok := comm.X.(*ast.UnaryExpr); ok {
					caseRecv[u] = true
				}
			case *ast.AssignStmt:
				if u, ok := comm.Rhs[0].(*ast.UnaryExpr); ok {
					caseRecv[u] = true
				}
			}
		}
		if !hasDefault {
			bad("runSession 里有不带 default 的 select（%s）：轮询线程不得阻塞", fset.Position(s.Pos()))
		}
		return true
	})
	ast.Inspect(run, func(x ast.Node) bool {
		if u, ok := x.(*ast.UnaryExpr); ok && u.Op == token.ARROW && !caseRecv[u] {
			bad("runSession 里有不在 select 的 case 上的接收 %s：轮询线程不得阻塞", str(u))
		}
		return true
	})
	takes := named(run, "takeResult")
	if len(takes) != 3 || len(named(run, "Results")) != 3 {
		bad("结果通道应只经 takeResult 取，换歌分支、补取块、每拍各一次；实得 takeResult %d 处、Results %d 处",
			len(takes), len(named(run, "Results")))
	}
	for _, c := range takes {
		if len(c.Args) != 4 || str(c.Args[0]) != "&lyricSt" || str(c.Args[1]) != "reader.Results()" ||
			str(c.Args[2]) != "assocKey.hasIDs()" || str(c.Args[3]) != "offsetSec" {
			bad("takeResult 的实参应为 (&lyricSt, reader.Results(), assocKey.hasIDs(), offsetSec)，实得 %s", str(c))
		}
	}

	// ---- takeResult：一个带 default 的 select，不循环 ----
	var sel []*ast.SelectStmt
	ast.Inspect(take, func(x ast.Node) bool {
		switch x := x.(type) {
		case *ast.SelectStmt:
			sel = append(sel, x)
		case *ast.ForStmt, *ast.RangeStmt:
			bad("takeResult 里不得有循环：每次至多取一条")
		}
		return true
	})
	if len(sel) != 1 {
		bad("takeResult 里应恰有一个 select，实得 %d", len(sel))
	} else {
		recv, dflt := false, false
		for _, c := range sel[0].Body.List {
			cc := c.(*ast.CommClause)
			if cc.Comm == nil {
				dflt = true
				continue
			}
			if a, ok := cc.Comm.(*ast.AssignStmt); ok && str(a.Rhs[0]) == "<-results" {
				recv = true
				if len(named(cc, "st.apply")) != 1 {
					bad("takeResult 收到结果后应交给 apply 一次")
				}
			}
		}
		if !recv || !dflt {
			bad("takeResult 的 select 应为 case res := <-results 与 default 两支（收到 %v，default %v）", recv, dflt)
		}
	}
	noForbidden("takeResult ", take)

	// ---- 换歌分支 ----
	var sw, fill, tick *ast.IfStmt
	pauseIdx, progIdx, tickIdx := -1, -1, -1
	for i, s := range loop.Body.List {
		switch s := s.(type) {
		case *ast.IfStmt:
			cond := str(s.Cond)
			switch {
			case sw == nil && len(named(s, "progressCarriedOver")) > 0:
				sw = s
			case fill == nil && strings.Contains(cond, "lyricsPending"):
				fill = s
			case cond == "v.pause":
				pauseIdx = i
			case len(named(s, "takeResult")) > 0:
				tick, tickIdx = s, i
			}
		case *ast.AssignStmt:
			if len(s.Lhs) == 1 && str(s.Lhs[0]) == "progressSec" {
				progIdx = i
			}
		}
	}
	if sw == nil || fill == nil || tick == nil || pauseIdx < 0 || progIdx < 0 {
		return append(probs, "没找到换歌分支、补取块、每拍取结果的那一块、暂停检测或 progressSec —— 门禁失效")
	}
	for _, name := range []string{"lyricSt.reset", "lyricSt.setOnlineRaw", "takeResult", "fetchLRC", "fetchCoverURL"} {
		if n := len(named(sw, name)); n != 1 {
			bad("换歌分支里 %s 应恰调一次，实得 %d", name, n)
		}
	}
	if n := len(named(sw, "reader.Submit")); n != 2 {
		bad("换歌分支里 reader.Submit 应恰两处（取词之前写入当前歌曲、取封面之后写入详情结果），实得 %d", n)
	}
	if !ordered(sw, "lyricSt.localPath", "lyricSt.reset", "reader.Submit", "fetchLRC", "fetchCoverURL", "lyricSt.setOnlineRaw", "takeResult") {
		bad("换歌分支的次序应为 取上一首的本地路径 → reset → Submit → fetchLRC → fetchCoverURL → setOnlineRaw → takeResult")
	}
	refreshAfterRaw := false
	for _, c := range named(sw, "lyricSt.refresh") {
		refreshAfterRaw = refreshAfterRaw || c.Pos() > first(sw, "lyricSt.setOnlineRaw") && c.Pos() < first(sw, "takeResult")
	}
	if !refreshAfterRaw {
		bad("换歌分支里 setOnlineRaw 与 takeResult 之间应有一次 refresh")
	}
	noIDs := false // 编号缺位那一支的直属语句里有 err = errSongMidUnavailable 与 lyricSt.refresh(…)
	ast.Inspect(sw, func(x ast.Node) bool {
		if s, ok := x.(*ast.IfStmt); ok {
			var stmts []string
			for _, st := range s.Body.List {
				switch st := st.(type) {
				case *ast.AssignStmt:
					stmts = append(stmts, str(st.Lhs[0])+" = "+str(st.Rhs[0]))
				case *ast.ExprStmt:
					stmts = append(stmts, str(st.X))
				}
			}
			joined := strings.Join(stmts, "\n")
			noIDs = noIDs || strings.Contains(joined, "err = errSongMidUnavailable") &&
				strings.Contains(joined, "lyricSt.refresh(assocKey.hasIDs(), offsetSec)")
		}
		return true
	})
	if !noIDs {
		bad("换歌分支编号缺位的一支应 lyricSt.refresh(assocKey.hasIDs(), offsetSec)，登记「无」已给出，免得读取者随后的「无」多发 index:-1")
	}
	keyOK := false
	for _, a := range assigns(sw, "assocKey", "") {
		if cl, ok := a.Rhs[0].(*ast.CompositeLit); ok && str(cl.Type) == "songKey" {
			keyOK = field(cl, "gen") == "assocKey.gen + 1" && field(cl, "excludeLocal") == "excludeFor(prevLocal, renamed)" &&
				(field(cl, "reopen") == "" || field(cl, "reopen") == "0")
		}
	}
	if !keyOK {
		bad("换歌分支应写 assocKey = songKey{gen: assocKey.gen + 1, …, excludeLocal: excludeFor(prevLocal, renamed), …}，不带 reopen（gen 加一时 reopen 归零）")
	}
	excludes := 0 // runSession 里名为 excludeLocal 的标识符：只许是上面那个字面量的键
	ast.Inspect(run, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok && id.Name == "excludeLocal" {
			excludes++
		}
		return true
	})
	if excludes != 1 {
		bad("runSession 里 excludeLocal 应只出现在换歌分支的 songKey 字面量里（只经 excludeFor 交出，补取块与重开不改它），实得 %d 处", excludes)
	}
	// 改名证据（#218 第 5 版，设计 §6.8「改名证据」）：换歌分支的直属语句依次取上一首的本地文件、求证据、打 Detail、写入、reset。
	const claimStmt = "renamed := loadOpen.claim(loadUsable, mem.Offsets().PlayState, pollInterval)"
	claimSeq := []string{"prevLocal := lyricSt.localPath()", claimStmt, "logRenameEvidence(prevLocal, renamed)", "assocKey = songKey{",
		"lyricSt.reset(assocKey.gen)"}
	claimOK := false
	for i := 0; i+len(claimSeq) <= len(sw.Body.List); i++ {
		ok := true
		for j, want := range claimSeq {
			got := stmt(sw.Body.List[i+j])
			ok = ok && (got == want || strings.HasSuffix(want, "{") && strings.HasPrefix(got, want))
		}
		claimOK = claimOK || ok
	}
	if !claimOK || len(named(run, "loadOpen.claim")) != 1 || len(named(sw, "loadOpen.claim")) != 1 ||
		len(named(run, "lyricSt.localPath")) != 1 || len(named(sw, "lyricSt.localPath")) != 1 {
		bad("换歌分支的直属语句应依次为 prevLocal := lyricSt.localPath()、%s、logRenameEvidence(prevLocal, renamed)、"+
			"assocKey = songKey{…}、lyricSt.reset(assocKey.gen)；loadOpen.claim 与 lyricSt.localPath 在 runSession 里各恰一处、"+
			"都在换歌分支（实得 claim %d 处、localPath %d 处）", claimStmt, len(named(run, "loadOpen.claim")), len(named(run, "lyricSt.localPath")))
	}
	if got := ifWithCond(sw, "err == nil"); got == nil || len(named(got, "lyricSt.setOnlineRaw")) != 1 || len(named(got, "takeResult")) != 1 {
		bad("换歌分支的 setOnlineRaw 与 takeResult 应在 if err == nil 里（取词成功才存在线原始行）")
	}
	if emits := lyricEmits(sw); len(emits) != 3 {
		bad("换歌分支里应有 3 处发歌词（无编号、零行、有歌词），实得 %d", len(emits))
	} else {
		for _, want := range []string{"isPaused = false", "lastLineIdx = -1"} { // 换歌分支的直属语句，不算等 songMid 时那一处
			found := false
			for _, s := range sw.Body.List {
				if a, ok := s.(*ast.AssignStmt); ok && len(a.Lhs) == 1 && str(a.Lhs[0])+" = "+str(a.Rhs[0]) == want {
					found = found || a.Pos() < emits[0].Pos()
				}
			}
			if !found {
				bad("换歌分支发歌词之前应先 %s", want)
			}
		}
	}

	// ---- 补取块 ----
	if up := ifWithCond(fill, "!assocKey.hasIDs()"); up == nil {
		bad("补取块里应有 if !assocKey.hasIDs()：编号由缺到有时 gen 加一并写入")
	} else {
		inc, zero, clear := token.NoPos, token.NoPos, token.NoPos
		ast.Inspect(up, func(x ast.Node) bool {
			if s, ok := x.(ast.Stmt); ok {
				switch stmt(s) {
				case "assocKey.gen++":
					inc = s.Pos()
				case "assocKey.reopen = 0":
					zero = s.Pos()
				case "assocKey.detail = songDetail{}":
					clear = s.Pos()
				}
			}
			return true
		})
		if clear == token.NoPos || clear < inc || clear > first(up, "reader.Submit") {
			bad("补取块的 if !assocKey.hasIDs() 里 assocKey.gen++ 之后、Submit 之前应有 assocKey.detail = songDetail{}" +
				"（编号由缺到有时详情结果为尚无，不沿用窗口用尽时记下的已失败）")
		}
		if inc == token.NoPos || !ordered(up, "lyricSt.reset", "reader.Submit") || up.End() > first(fill, "fetchLRC") {
			bad("补取块的 if !assocKey.hasIDs() 里应 assocKey.gen++、reset、Submit，且都在 fetchLRC 之前")
		}
		if zero == token.NoPos || zero < inc || zero > first(up, "reader.Submit") {
			bad("补取块的 if !assocKey.hasIDs() 里 assocKey.gen++ 之后、Submit 之前应有 assocKey.reopen = 0（gen 加一时 reopen 归零）")
		}
	}
	success := ifWithCond(fill, "ferr == nil")
	if success == nil || !ordered(success, "lyricSt.setOnlineRaw", "lyricSt.refresh", "takeResult", "showLyrics") {
		bad("补取块取词成功后应依次 setOnlineRaw → refresh → takeResult → showLyrics")
	} else {
		direct := false // showLyrics 是取词成功分支的直属语句：每次补取成功都发，标题与时长随之更新
		for _, s := range success.Body.List {
			direct = direct || stmt(s) == "showLyrics(shown, float32(meta.ProgressMs) / 1000.0, sampleTime)"
		}
		keep := ifWithCond(success, "!changed")
		if !direct || keep == nil || len(keep.Body.List) != 1 || stmt(keep.Body.List[0]) != "shown = currentLyrics" ||
			keep.Pos() > first(success, "showLyrics") {
			bad("补取成功应总经 showLyrics 发出（不放在条件里），显示的那份不变时先 if !changed { shown = currentLyrics }")
		}
	}
	var giveUp *ast.CaseClause // 补取窗口用尽而放弃的子句（#227）
	ast.Inspect(fill, func(x ast.Node) bool {
		if cc, ok := x.(*ast.CaseClause); ok && len(cc.List) == 1 && str(cc.List[0]) == "backfillGiveUp" {
			giveUp = cc
		}
		return true
	})
	pendingAssigns := func(n ast.Node) int { return len(assigns(n, "lyricsPending", "")) }
	direct := func(list []ast.Stmt) int {
		n := 0
		for _, st := range list {
			if a, ok := st.(*ast.AssignStmt); ok && len(a.Lhs) == 1 && str(a.Lhs[0]) == "lyricsPending" && str(a.Rhs[0]) == "false" {
				n++
			}
		}
		return n
	}
	switch {
	case giveUp == nil || success == nil:
		bad("补取块里没找到 case backfillGiveUp 或取词成功的分支")
	case pendingAssigns(fill) != 2 || direct(giveUp.Body) != 1 || direct(success.Body.List) != 1 ||
		pendingAssigns(run) != pendingAssigns(sw)+pendingAssigns(fill):
		bad("lyricsPending 的赋值应与 main 的补取块相同：只在 case backfillGiveUp 与取词成功处各置 false 一次，换歌分支与补取块之外不赋值")
	default:
		local := ifWithCond(giveUp, "lyricSt.source() == srcLocal")
		timeout := func(n ast.Node, level string) bool {
			for _, c := range named(n, level) {
				if len(c.Args) > 0 && strings.HasPrefix(str(c.Args[0]), `"Lyric backfill timed out`) {
					return true
				}
			}
			return false
		}
		if local == nil || !timeout(local.Body, "Detail") || local.Else == nil || !timeout(local.Else, "Warn") {
			bad("补取超时应在来源为本地文件时走 log.Detail，否则 log.Warn")
		}
	}

	// ---- 详情结果的交接（设计 §6.4 第 2 步「交接」、§6.8「换歌拍」「补取块」）----
	// handoff 报告 s 是否为 if k, ok := <call>; ok { assocKey = k; reader.Submit(assocKey) }，没有 else。
	handoff := func(s ast.Stmt, call string) bool {
		ifs, ok := s.(*ast.IfStmt)
		if !ok || ifs.Else != nil || str(ifs.Cond) != "ok" || len(ifs.Body.List) != 2 ||
			stmt(ifs.Body.List[0]) != "assocKey = k" || stmt(ifs.Body.List[1]) != "reader.Submit(assocKey)" {
			return false
		}
		a, ok := ifs.Init.(*ast.AssignStmt)
		return ok && a.Tok == token.DEFINE && len(a.Lhs) == 2 && len(a.Rhs) == 1 &&
			str(a.Lhs[0]) == "k" && str(a.Lhs[1]) == "ok" && str(a.Rhs[0]) == call
	}
	// coverHandoff 报告 n 里 …, det = fetchCoverURL(ids) 那条语句是否紧跟以 applyDetail(assocKey, ids, det) 为条件的写入。
	coverHandoff := func(n ast.Node, ids string) bool {
		found := false
		ast.Inspect(n, func(x ast.Node) bool {
			if b, ok := x.(*ast.BlockStmt); ok {
				for i, s := range b.List[:max(len(b.List)-1, 0)] {
					a, ok := s.(*ast.AssignStmt)
					if ok && len(a.Lhs) == 2 && len(a.Rhs) == 1 && str(a.Rhs[0]) == "fetchCoverURL("+ids+")" && str(a.Lhs[1]) != "_" {
						found = found || handoff(b.List[i+1], "applyDetail(assocKey, "+ids+", "+str(a.Lhs[1])+")")
					}
				}
			}
			return true
		})
		return found
	}
	if n := len(named(run, "fetchCoverURL")); n != 2 {
		bad("runSession 里 fetchCoverURL 应恰两处（换歌分支与补取块，位置同前），不为补全编号另加请求；实得 %d", n)
	}
	type coverSite struct {
		where string
		n     ast.Node
		ids   string // 所查编号
	}
	covers := []coverSite{{"换歌分支", sw, "meta.SongID, meta.SongMid"}}
	if success != nil {
		if len(named(success, "fetchCoverURL")) != 1 || first(success, "fetchCoverURL") < first(success, "showLyrics") {
			bad("补取块的 fetchCoverURL 应在取词成功的分支里、showLyrics 之后（位置同前）")
		}
		covers = append(covers, coverSite{"补取块", success, "sid, smid"})
	}
	for _, c := range covers {
		if !coverHandoff(c.n, c.ids) {
			bad("%s的 …, det = fetchCoverURL(%s) 之后应紧跟 if k, ok := applyDetail(assocKey, %s, det); ok { assocKey = k; reader.Submit(assocKey) }",
				c.where, c.ids, c.ids)
		}
	}
	if success != nil {
		// 补取每拍重读编号，所查编号可以与 assocKey 的不同而衔接不上：取词成功即结束补取，详情结果仍为尚无就再无写入。
		found := false
		for i, s := range success.Body.List[:max(len(success.Body.List)-1, 0)] {
			found = found || handoff(s, "applyDetail(assocKey, sid, smid, det)") && handoff(success.Body.List[i+1], "exhaustDetail(assocKey)")
		}
		if !found {
			bad("补取块取词成功的分支里，applyDetail 的写入之后应紧跟 if k, ok := exhaustDetail(assocKey); ok { assocKey = k; reader.Submit(assocKey) }" +
				"（所查编号衔接不上时详情结果不滞留尚无）")
		}
	}
	var hold *ast.CaseClause // 补取窗口用尽而暂停中保持的子句（#227）
	ast.Inspect(fill, func(x ast.Node) bool {
		if cc, ok := x.(*ast.CaseClause); ok && len(cc.List) == 1 && str(cc.List[0]) == "backfillHold" {
			hold = cc
		}
		return true
	})
	for _, c := range []struct {
		name string
		cc   *ast.CaseClause
	}{{"backfillGiveUp", giveUp}, {"backfillHold", hold}} {
		found := false
		if c.cc != nil {
			for _, s := range c.cc.Body {
				found = found || handoff(s, "exhaustDetail(assocKey)")
			}
		}
		if !found {
			bad("补取块的 case %s 里应有 if k, ok := exhaustDetail(assocKey); ok { assocKey = k; reader.Submit(assocKey) }（窗口用尽时尚无记为已失败）", c.name)
		}
	}
	if n := len(named(run, "reader.Submit")); n != 8 {
		bad("reader.Submit 应恰 8 处：换歌分支、补取块编号由缺到有、reopenSong，加详情结果的五处写入（两处取封面之后、补取成功之后仍为尚无、"+
			"补取放弃与保持），实得 %d", n)
	}

	// ---- 每拍 ----
	if !(pauseIdx < tickIdx && tickIdx < progIdx) {
		bad("每拍取结果应在暂停检测之后、行匹配之前（暂停检测 %d、取结果 %d、progressSec %d）", pauseIdx, tickIdx, progIdx)
	}
	if str(tick.Cond) != `lastName != ""` {
		bad(`每拍取结果的条件应为 lastName != ""（认领之前不取），实得 %s`, str(tick.Cond))
	}
	if cs := named(tick, "showLyrics"); len(cs) != 1 || len(cs[0].Args) != 3 ||
		str(cs[0].Args[1]) != "float32(anchorProgressMs) / 1000.0" || str(cs[0].Args[2]) != "anchorTime" {
		bad("每拍应以锚点经 showLyrics(lines, float32(anchorProgressMs) / 1000.0, anchorTime) 发出")
	}
	noForbidden("每拍取结果的那一块", tick)

	// ---- 补发暂停：入队失败时每拍再试，恢复与换歌时清掉 ----
	retryIdx := -1
	for i, s := range loop.Body.List {
		if s, ok := s.(*ast.IfStmt); ok && str(s.Cond) == "repausePending && isPaused" &&
			len(s.Body.List) == 1 && stmt(s.Body.List[0]) == "repausePending = !p.emitFrozenPause(repausePos, currentDurationSec)" {
			retryIdx = i
		}
	}
	if !(pauseIdx < retryIdx && retryIdx < progIdx) {
		bad("暂停检测之后、行匹配之前应有 if repausePending && isPaused { repausePending = !p.emitFrozenPause(repausePos, currentDurationSec) }")
	}
	clears, unpaired := 0, 0
	ast.Inspect(run, func(x ast.Node) bool {
		if b, ok := x.(*ast.BlockStmt); ok {
			for i, s := range b.List {
				if stmt(s) == "isPaused = false" {
					clears++
					if i+1 >= len(b.List) || stmt(b.List[i+1]) != "repausePending = false" {
						unpaired++
					}
				}
			}
		}
		return true
	})
	// 第 5 轮（#222）另有暂停中跳转的补发（if v.pausedSeek），repausePending、repausePos 各多一处赋值，其形状由
	// playstate_test.go 的接线门禁 m 守。
	if clears < 2 || unpaired > 0 || len(assigns(run, "repausePending", "")) != clears+3 || len(assigns(run, "repausePos", "")) != 2 {
		bad("每处 isPaused = false（换歌、恢复）之后应紧跟 repausePending = false；repausePending 只在这些地方与三处补发（showLyrics、"+
			"暂停中跳转、再试）时赋值，repausePos 只在 showLyrics 与暂停中跳转里赋值（%d 处 isPaused = false，%d 处没有紧跟）", clears, unpaired)
	}
	if frozen == nil {
		bad("没找到 emitFrozenPause")
	} else {
		tries := named(frozen, "TryEmit")
		if len(tries) != 1 || len(named(frozen, "Emit")) != 0 || len(tries[0].Args) != 2 ||
			str(tries[0].Args[0]) != "player.EventPlaybackPause" || field(tries[0].Args[1], "Position") != "posSec" {
			bad("emitFrozenPause 应以 TryEmit 在 posSec 发 playback_pause 并返回是否入队（Emit 满了静默丢弃）")
		}
	}

	// ---- showLyrics ----
	if showIdx > loopIdx {
		bad("showLyrics 应在轮询循环之前定义")
	}
	const repauseCall = "repausePending = !p.emitFrozenPause(repausePos, currentDurationSec)"
	undecidedIdx, earlyIdx, resetIdx, posIdx, emitIdx, freezeIdx := -1, -1, -1, -1, -1, -1
	for i, s := range show.Body.List {
		switch s := s.(type) {
		case *ast.AssignStmt:
			if stmt(s) == "undecided := lyricsUndecided(lines, lyricsPending, lyricSt.source())" {
				undecidedIdx = i
			}
			if stmt(s) == "lastLineIdx = -1" {
				resetIdx = i
			}
			if c, ok := s.Rhs[0].(*ast.CallExpr); ok && len(s.Lhs) == 1 && str(s.Lhs[0]) == "pos" &&
				str(c.Fun) == "player.LivePosition" && len(c.Args) == 5 && str(c.Args[3]) == "!isPaused" {
				posIdx = i
			}
		case *ast.IfStmt:
			switch {
			case str(s.Cond) == "undecided && len(currentLyrics) == 0" && len(s.Body.List) == 1:
				if _, ok := s.Body.List[0].(*ast.ReturnStmt); ok {
					earlyIdx = i
				}
			case str(s.Cond) == "isPaused" && len(s.Body.List) == 2 &&
				stmt(s.Body.List[0]) == "repausePos = pos" && stmt(s.Body.List[1]) == repauseCall:
				freezeIdx = i
			case len(lyricEmits(s)) == 2:
				emitIdx = i
				if str(s.Cond) != "len(lines) == 0 && !undecided" || len(named(s.Body, "emitNoLyrics")) != 1 {
					bad("showLyrics 应在 len(lines) == 0 && !undecided 时走 emitNoLyrics，否则发 all_lyrics（没有结论时不发 index:-1）")
				}
			}
		}
	}
	switch {
	case emitIdx < 0:
		bad("showLyrics 里应有一个 if：行为空走 emitNoLyrics，否则发 all_lyrics")
	case resetIdx < 0 || resetIdx > emitIdx:
		bad("showLyrics 发歌词之前应先 lastLineIdx = -1")
	case undecidedIdx < 0 || earlyIdx < undecidedIdx || resetIdx < earlyIdx:
		bad("showLyrics 开头应先 undecided := lyricsUndecided(lines, lyricsPending, lyricSt.source())，" +
			"再 if undecided && len(currentLyrics) == 0 { return }（显示本就为空时什么也不发），都在改动状态之前")
	case posIdx < 0 || posIdx > emitIdx:
		bad("showLyrics 发歌词之前应以 pos := player.LivePosition(…, !isPaused, …) 取位置")
	case freezeIdx < emitIdx:
		bad("showLyrics 发歌词之后应有 if isPaused { repausePos = pos; " + repauseCall + " }（与 all_lyrics 同一位置，歌词与无歌词两支都要）")
	}
	if emitIdx >= 0 {
		for _, c := range lyricEmits(show.Body.List[emitIdx]) {
			at := ""
			switch {
			case len(c.Args) == 4: // emitNoLyrics(title, durationSec, positionSec, progress)
				at = str(c.Args[2])
			case len(c.Args) == 2: // Emit(player.EventAllLyrics, &player.AllLyricsData{…})
				at = field(c.Args[1], "Position")
			}
			if at != "pos" {
				bad("showLyrics 发歌词的位置应取 pos，实得 %s", str(c))
			}
		}
	}
	noForbidden("showLyrics ", show)

	// ---- 发歌词的地方 ----
	if n := len(lyricEmits(run)); n != 5 {
		bad("runSession 里发歌词的应只有换歌分支（3 处）与 showLyrics（2 处），实得 %d 处", n)
	}
	if n := len(named(run, "showLyrics")); n != 2 {
		bad("showLyrics 应只在补取块与每拍各调一次，实得 %d 次", n)
	}

	// ---- 读当前歌曲记录只在读取者里 ----
	for _, name := range recordReaders {
		if len(named(run, name)) > 0 {
			bad("runSession 里调用了 %s：读当前歌曲记录只在读取者里，会话只复制只读句柄交给它", name)
		}
	}
	for _, name := range fillQueries {
		if len(named(run, name)) > 0 {
			bad("runSession 里调用了 %s：编号兜底只在读取者里，会话只交出封面请求带回的详情结果", name)
		}
	}

	// ---- 同一首重开（设计 §6.8「重开」）----
	const (
		usableStmt = "loadUsable := mem.Offsets().PlayState.loadingUsable(judge.bad, pollInterval)"
		edgeStmt   = "edge := loadOpen.step(mem.Offsets().PlayState, meta.PlayState, meta.PlayStateOK, sampleTime)"
		loadCond   = "!reopenedThisTick && reopenByLoading(loadUsable, edge, meta.Name, lastName, assocKey.hasIDs())"
		holdCond   = "!reopenedThisTick && !loadUsable && released && meta.ProgressMs < lastMemProgress && assocKey.hasIDs()"
	)
	var reopenFn *ast.FuncLit
	for i, s := range body {
		if a, ok := s.(*ast.AssignStmt); ok && len(a.Lhs) == 1 && len(a.Rhs) == 1 && str(a.Lhs[0]) == "reopenSong" {
			if fl, ok := a.Rhs[0].(*ast.FuncLit); ok && i < loopIdx {
				reopenFn = fl
			}
		}
	}
	if reopenFn == nil {
		bad("轮询循环之前应定义 reopenSong := func(…) { … }")
	} else {
		var got []string
		for _, s := range reopenFn.Body.List[:min(3, len(reopenFn.Body.List))] {
			got = append(got, stmt(s))
		}
		gen := false
		ast.Inspect(reopenFn, func(x ast.Node) bool {
			if se, ok := x.(*ast.SelectorExpr); ok && str(se) == "assocKey.gen" {
				gen = true
			}
			return true
		})
		if strings.Join(got, "; ") != "assocKey.reopen++; lyricSt.reopened(assocKey.reopen); reader.Submit(assocKey)" ||
			len(named(reopenFn, "reset")) > 0 || gen {
			bad("reopenSong 应依次 assocKey.reopen++、lyricSt.reopened(assocKey.reopen)、reader.Submit(assocKey)，"+
				"不 reset、不碰 assocKey.gen（重开只换代次），实得开头 %q", got)
		}
	}
	reopenIncs := 0
	ast.Inspect(run, func(x ast.Node) bool {
		if s, ok := x.(*ast.IncDecStmt); ok && str(s.X) == "assocKey.reopen" {
			reopenIncs++
		}
		return true
	})
	if reopenIncs != 1 || len(named(run, "lyricSt.reopened")) != 1 {
		bad("assocKey.reopen 的改动与 lyricSt.reopened 应只在 reopenSong 里各一处（实得 reopen 自增 %d 处、reopened %d 处）",
			reopenIncs, len(named(run, "lyricSt.reopened")))
	}
	if n := len(assigns(run, "loadUsable", "")); n != 1 {
		bad("loadUsable 应恰赋值一次（每拍开头一次，四处写入的条件都出自它），实得 %d 处", n)
	}
	// 计数：首读与两处重读各 step 一次、各是一个载入支（loadOpen.step 与 reopenByLoading 都恰三处），加上暂扣支，reopenSong
	// 恰四处。
	if n, m := len(named(run, "loadOpen.step")), len(named(run, "reopenByLoading")); n != 3 || m != 3 {
		bad("loadOpen.step 与 reopenByLoading 应各恰调三次（每拍开头的首读、换歌认领与补取的重读各一处载入支），实得 %d、%d", n, m)
	}
	// 每拍至多写入一次：reopenedThisTick 在循环体里以 := false 声明一次，只在四处写入时置真，别处不赋值。
	if n := len(assigns(run, "reopenedThisTick", "")); n != 5 || len(assigns(run, "reopenedThisTick", "true")) != 4 {
		bad("reopenedThisTick 应只有循环体里的一处 := false 与四处写入时的 = true，实得 %d 处赋值", n)
	}
	// loadSite 认载入支：if edgeStmt; loadCond { reopenedThisTick = true; reopenSong(true) }，没有 else。
	loadSite := func(s ast.Stmt) bool {
		ifs, ok := s.(*ast.IfStmt)
		return ok && ifs.Init != nil && stmt(ifs.Init) == edgeStmt && str(ifs.Cond) == loadCond && ifs.Else == nil &&
			len(ifs.Body.List) == 2 && stmt(ifs.Body.List[0]) == "reopenedThisTick = true" && stmt(ifs.Body.List[1]) == "reopenSong(true)"
	}
	// rereadStep 返回 n 里取词之后的重读（以 rereadSameSong 为条件的 if）被采用的那一支里，换上重读的播放状态之后的语句；
	// 没有这样的 if 或其中没有换上播放状态的语句时为 nil。
	rereadStep := func(n ast.Node) []ast.Stmt {
		var after []ast.Stmt
		ast.Inspect(n, func(x ast.Node) bool {
			ifs, ok := x.(*ast.IfStmt)
			if !ok || after != nil {
				return after == nil
			}
			if c, ok := ifs.Cond.(*ast.CallExpr); ok && str(c.Fun) == "rereadSameSong" {
				for i, s := range ifs.Body.List {
					if a, ok := s.(*ast.AssignStmt); ok && len(a.Lhs) == 2 && str(a.Lhs[0])+", "+str(a.Lhs[1]) == "meta.PlayState, meta.PlayStateOK" {
						after = append([]ast.Stmt{}, ifs.Body.List[i+1:]...)
					}
				}
			}
			return after == nil
		})
		return after
	}
	sampleIdx, usableIdx, onceIdx, loadIdx, swIdx, stepIdx := -1, -1, -1, -1, -1, -1
	var change *ast.IfStmt
	for i, s := range loop.Body.List {
		switch s := s.(type) {
		case *ast.AssignStmt:
			switch stmt(s) {
			case "sampleTime := time.Now()":
				sampleIdx = i
			case usableStmt:
				usableIdx = i
			case "reopenedThisTick := false":
				onceIdx = i
			}
			if len(named(s, "hold.step")) > 0 {
				stepIdx = i
			}
		case *ast.IfStmt:
			switch {
			case s == sw:
				swIdx = i
			case loadSite(s):
				loadIdx = i
			case str(s.Cond) == "meta.ProgressMs != lastMemProgress":
				change = s
			}
		}
	}
	switch {
	case usableIdx < 0:
		bad("轮询循环体里应有直属的 %s（每拍开头一次）", usableStmt)
	case onceIdx < 0:
		bad("轮询循环体里应有直属的 reopenedThisTick := false（每拍至多写入一次重开）")
	case loadIdx < 0:
		bad("轮询循环体里应有直属的 if %s; %s { reopenedThisTick = true; reopenSong(true) }（每拍以首读 step）", edgeStmt, loadCond)
	case !(sampleIdx >= 0 && sampleIdx < usableIdx && sampleIdx < onceIdx && usableIdx < loadIdx && onceIdx < loadIdx &&
		loadIdx < swIdx && loadIdx < stepIdx):
		bad("载入支应在 sampleTime 之后、换歌检测与 hold.step 之前（sampleTime %d、loadUsable %d、reopenedThisTick %d、载入支 %d、换歌 %d、hold.step %d）",
			sampleIdx, usableIdx, onceIdx, loadIdx, swIdx, stepIdx)
	}
	// 换歌认领与补取两处取词之后的重读被采用时，都以重读的播放状态 step、按同样的条件写入（写法相同）：认领拍首读不是载入值、
	// 重读才见到，即认领之后开始了一次新的载入，多半是重开。
	for _, at := range []struct {
		where string
		n     ast.Node
	}{{"换歌分支", sw}, {"补取块", fill}} {
		found := false
		for _, s := range rereadStep(at.n) {
			found = found || loadSite(s)
		}
		if !found {
			bad("%s取词之后的重读被采用、换上播放状态之后，应有 if %s; %s { reopenedThisTick = true; reopenSong(true) }", at.where, edgeStmt, loadCond)
		}
	}
	holdOK := false
	if change != nil {
		if seek := ifWithCond(change, "seekDetected"); seek != nil {
			if h := ifWithCond(seek, holdCond); h != nil && len(h.Body.List) == 2 && stmt(h.Body.List[0]) == "reopenedThisTick = true" &&
				stmt(h.Body.List[1]) == "reopenSong(false)" {
				holdOK = true
			}
		}
	}
	if !holdOK {
		bad("变化分支的 if seekDetected 里应有 if %s { reopenedThisTick = true; reopenSong(false) }（暂扣放行按回跳补发，载入信号不可用时）", holdCond)
	}
	if n := len(named(run, "reopenSong")); n != 4 {
		bad("reopenSong 应恰四处调用（拍首、换歌重读、补取重读的载入支与暂扣支），实得 %d", n)
	}
	for _, fd := range []*ast.FuncDecl{loadStep, judgeFn} {
		if fd == nil || len(named(fd.Body, "classify")) == 0 {
			bad("loadEdge.step 与 judgeTick 都应经 classify 判取值（同一分类函数），没找到其一或其中没有 .classify(")
			break
		}
	}
	const byLoadingRet = `usable && edge && lastName != "" && name == lastName && hasIDs`
	if byLoading == nil || len(byLoading.Body.List) != 1 {
		bad("没找到 reopenByLoading，或它不是单条 return")
	} else if r, ok := byLoading.Body.List[0].(*ast.ReturnStmt); !ok || len(r.Results) != 1 || str(r.Results[0]) != byLoadingRet {
		bad("reopenByLoading 应为 return %s", byLoadingRet)
	}

	// ---- 改名证据的窗口与载入段的记录（#218 第 5 版，设计 §6.8「改名证据」）----
	// loadOpen 只由函数体直属的一处 newLoadEdge 建出（窗口与载入段的状态只此一份，claim 与三处 step 出自同一个实例）。
	const newEdgeStmt = "loadOpen := newLoadEdge(pollInterval)"
	pollIdx, newIdx, newDirect := -1, -1, 0
	for i, s := range body {
		switch stmt(s) {
		case "pollInterval := EffectivePollInterval(p.pollMs)":
			pollIdx = i
		case newEdgeStmt:
			newIdx, newDirect = i, newDirect+1
		}
	}
	if newDirect != 1 || !(pollIdx >= 0 && pollIdx < newIdx && newIdx < loopIdx) {
		bad("runSession 的直属语句里应恰有一处 %s，在 pollInterval := EffectivePollInterval(p.pollMs) 之后、轮询循环之前", newEdgeStmt)
	}
	edgeLits, edgeVars := 0, 0
	ast.Inspect(run, func(x ast.Node) bool {
		switch x := x.(type) {
		case *ast.CompositeLit:
			if x.Type != nil && str(x.Type) == "loadEdge" {
				edgeLits++
			}
		case *ast.ValueSpec:
			if x.Type != nil && strings.TrimPrefix(str(x.Type), "*") == "loadEdge" {
				edgeVars++
			}
		}
		return true
	})
	if n, m := len(assigns(run, "loadOpen", "")), len(named(run, "newLoadEdge")); n != 1 || m != 1 || edgeLits > 0 || edgeVars > 0 {
		bad("loadEdge 应只经 %s 建出一次：loadOpen 的赋值 %d 处、newLoadEdge %d 次、loadEdge{…} %d 处、类型为 loadEdge 的 var %d 处",
			newEdgeStmt, n, m, edgeLits, edgeVars)
	}
	// 每拍开头整拍元数据读失败而 continue 的分支补记一次读不成（它不调用 step）。
	missedOK := false
	for i, s := range loop.Body.List[:max(len(loop.Body.List)-1, 0)] {
		a, ok := s.(*ast.AssignStmt)
		if !ok || len(a.Lhs) != 2 || len(a.Rhs) != 1 || str(a.Lhs[0])+", "+str(a.Lhs[1]) != "meta, err" || str(a.Rhs[0]) != "mem.ReadAllMetadata()" {
			continue
		}
		if ifs, ok := loop.Body.List[i+1].(*ast.IfStmt); ok && ifs.Init == nil && ifs.Else == nil && str(ifs.Cond) == "err != nil" &&
			len(ifs.Body.List) == 2 && stmt(ifs.Body.List[0]) == "loadOpen.missed()" {
			br, ok := ifs.Body.List[1].(*ast.BranchStmt)
			missedOK = ok && br.Tok == token.CONTINUE && br.Label == nil
		}
	}
	if !missedOK || len(named(run, "loadOpen.missed")) != 1 {
		bad("轮询循环体里紧跟 meta, err := mem.ReadAllMetadata() 的 if err != nil 应恰为 loadOpen.missed() 与 continue 两句，" +
			"loadOpen.missed 全函数恰一处（这一拍没有读到播放状态，改名证据不成立）")
	}
	return probs
}

// TestRunSessionAssocWiring 钉住 runSession 接入歌词关联的接线形状（见 assocWiringProblems）。门禁自身的变异见下一条。
//
// 变基到 #172 之后写进 qqmusic.go 的变异（均实测，在仓库外的副本里）：emitFrozenPause 改用 Emit 并恒报入队（另有
// frozenpause_test.go 红）、去掉入队失败的再试、再试挪到暂停与恢复判定之前、换歌或判恢复时不清待补发，各自只有本例
// （与依赖同一处原文的自测）红；暂停中补发本身的去掉、挪动与改写同时由 playstate_test.go 的接线门禁 d、g 拦下。
//
// 同一首重开（#218 第 3 版）写进 qqmusic.go 的变异（均实测，在仓库外的副本里）：补取块不把 reopen 归零、暂扣支不看
// loadUsable、reopenSong 以 reset 代替 reopened、reopenSong 不写入读取者，各自只有本例（与依赖同一处原文的自测）红；
// reopenByLoading 不比歌名、不看已认领，本例与 reopensignal_test.go 都红。
//
// 终审 F1、F2（取词之后的重读里的载入值）写进 qqmusic.go 的变异（均实测，在仓库外的副本里）：补取重读处漏调 loadOpen.step、
// 补取重读的载入支去掉本拍一次的约束或去掉 loadUsable、暂扣支去掉本拍一次的约束（F1）；换歌重读处漏调 step、只 step 不写、
// 去掉 loadUsable、去掉 reopenedThisTick（F2），各自只有本例（与依赖同一处原文的自测）红；两轮修复之前的 qqmusic.go 配本门禁
// 都红。行为由 reopenreread_test.go 的复述守。
//
// 详情结果的交接（#218 第 4 版 Task 17）写进 qqmusic.go 的变异（均实测，在仓库外的副本里）：去掉换歌分支或补取块的写入、换歌分支的
// 写入挪到 fetchCoverURL 之前、两处 applyDetail 的所查编号换成 assocKey 自己的、两处去掉 if 每次都写入、漏掉 case backfillHold 或
// case backfillGiveUp 的写入、backfillHold 里不经 exhaustDetail 直接写已失败、补取块为补全另加一次 fetchCoverURL、每拍取结果时调用
// fetchSongMidByID、补取块编号由缺到有时不清详情结果，各自只有本例（与依赖同一处原文的自测）红；TestRunSessionPlayStateWiring、
// 回放与补取窗口的用例全绿。行为由 detailhandoff_test.go 的复述守。
//
// 补取换了所查编号（#218 第 4 版终审第 2 条）写进 qqmusic.go 的变异（实测，在仓库外的副本里）：漏掉补取成功之后的 exhaustDetail，
// 本例（与依赖同一处原文的自测）红；行为由 detailhandoff_test.go 的 TestDetailHandoffBackfillSwitchesIDs 守。
//
// 改名证据（#218 第 5 版）写进 qqmusic.go 的变异（均实测，在仓库外的副本里）：claim 的 loadUsable 换成 true 或 pollInterval 换成 0、
// excludeLocal 写成 lyricSt.localPath() 或空串、去掉 logRenameEvidence、元数据读失败的分支去掉 missed 或把它挪到 continue 之后、
// 另建一个 loadEdge 实例求证据、三处 step 各自改传 time.Now()、loadOpen 改回 var loadOpen loadEdge、补取块编号由缺到有时清掉
// excludeLocal、换歌分支调两次 claim，各自只有本例（与依赖同一处原文的自测）红。行为由 reopenreread_test.go 的
// TestRenameEvidenceReplay 复述守。
//
// 暂停中跳转（#222）写进 qqmusic.go 的变异（实测，在仓库外的副本里）：删去 if v.pausedSeek 块、块里改用 p.Emit 直接发
// playback_pause，本例的赋值计数红（playstate_test.go 的接线门禁 m 同红）。
func TestRunSessionAssocWiring(t *testing.T) {
	if probs := assocWiringProblems(readQQMusicSource(t)); len(probs) > 0 {
		t.Fatalf("runSession 的歌词关联接线不符：\n  %s", strings.Join(probs, "\n  "))
	}
}

// TestAssocWiringCatchesKnownHoles 以改写过的源码确认 assocWiringProblems 拦得住各处接线变异：每种改写都要报出问题。
// 改写找不到原文时自报失效（源码变了，本条自测要跟着改）。改写只需能解析，不必能编译。
func TestAssocWiringCatchesKnownHoles(t *testing.T) {
	src := readQQMusicSource(t)
	replace := func(old, new string) func(string) string {
		return func(s string) string { return strings.Replace(s, old, new, 1) }
	}
	// replaceAfter 把 at 之后第一处 old 换成 new；找不到 at 时不改写。
	replaceAfter := func(at, old, new string) func(string) string {
		return func(s string) string {
			i := strings.Index(s, at)
			if i < 0 {
				return s
			}
			return s[:i] + strings.Replace(s[i:], old, new, 1)
		}
	}
	// move 删掉 block，再把它插在 at 之前（after 为真时插在 at 之后）；找不到 at 时不改写。
	move := func(block, at string, after bool) func(string) string {
		return func(orig string) string {
			s := strings.Replace(orig, block, "", 1)
			if !strings.Contains(s, at) {
				return orig
			}
			if after {
				return strings.Replace(s, at, at+block, 1)
			}
			return strings.Replace(s, at, block+at, 1)
		}
	}
	const (
		takeDefault   = "\tdefault:\n\t}\n\treturn lines, changed\n"
		claimSubmit   = "\t\t\treader.Submit(assocKey)\n"
		claimCover    = "\t\t\t\tcoverURL, det = fetchCoverURL(meta.SongID, meta.SongMid)\n"
		claimKey      = "\t\t\tassocKey = songKey{gen: assocKey.gen + 1, songID: sid, songMid: meta.SongMid, name: meta.Name, excludeLocal: excludeFor(prevLocal, renamed)}\n"
		claimTake     = "\t\t\t\t\tif lines, changed := takeResult(&lyricSt, reader.Results(), assocKey.hasIDs(), offsetSec); changed {\n\t\t\t\t\t\tcurrentLyrics = lines\n\t\t\t\t\t}\n"
		claimRaw      = "\t\t\t\tif err == nil {\n\t\t\t\t\tlyricSt.setOnlineRaw(currentLyrics)\n"
		claimNoIDs    = "\t\t\t\tlyricSt.refresh(assocKey.hasIDs(), offsetSec)\n\t\t\t} else {\n"
		fillSubmit    = "\t\t\t\t\t\treader.Submit(assocKey)\n"
		fillFetch     = "\t\t\t\t\tlines, _, _, ferr := fetchLRC(sid, smid, cookie, meta.DurationMs)\n"
		fillKeep      = "\t\t\t\t\t\tif !changed {\n\t\t\t\t\t\t\tshown = currentLyrics\n\t\t\t\t\t\t}\n"
		fillShow      = "\t\t\t\t\t\tshowLyrics(shown, float32(meta.ProgressMs)/1000.0, sampleTime)\n"
		fillAdopt     = "\t\t\t\t\t\t\tshown, changed = l, true\n"
		tickHead      = "\t\tif lastName != \"\" {\n\t\t\tif lines, changed := takeResult(&lyricSt, reader.Results(), assocKey.hasIDs(), offsetSec); changed {\n"
		tickShow      = "\t\t\t\tshowLyrics(lines, float32(anchorProgressMs)/1000.0, anchorTime)\n"
		tickBlock     = tickHead + tickShow + "\t\t\t}\n\t\t}\n"
		pauseHead     = "\t\t// --- 暂停与恢复判定（#172）---"
		showReset     = "\t\tcurrentLyrics = lines\n\t\tlastLineIdx = -1\n"
		showPos       = "pos := player.LivePosition(baseSec, at, time.Now(), !isPaused, currentDurationSec)"
		showFreeze    = "\t\tif isPaused {\n\t\t\trepausePos = pos\n\t\t\trepausePending = !p.emitFrozenPause(repausePos, currentDurationSec)\n\t\t}\n"
		showRecord    = "\t\t\trepausePending = !p.emitFrozenPause(repausePos, currentDurationSec)\n\t\t}\n\t}\n"
		showLyricsEnd = "\t\t\t\tCount: len(items), Lyrics: items,\n\t\t\t})\n"
		showNoLyrics  = "p.emitNoLyrics(currentTitle, currentDurationSec, pos, prog)"
		showEarly     = "\t\tif undecided && len(currentLyrics) == 0 {\n\t\t\treturn\n\t\t}\n"
		showNoLyrIf   = "if len(lines) == 0 && !undecided {"
		showUndecided = "lyricsUndecided(lines, lyricsPending, lyricSt.source())"
		retryBlock    = "\t\tif repausePending && isPaused {\n\t\t\trepausePending = !p.emitFrozenPause(repausePos, currentDurationSec)\n\t\t}\n"
		frozenTry     = "return p.TryEmit(player.EventPlaybackPause,"
		claimClear    = "\t\t\tisPaused = false\n\t\t\trepausePending = false\n\t\t\tjudge.resume.reset()\n"
		resumeClear   = "\t\t\tisPaused = false\n\t\t\trepausePending = false\n\t\t\tanchorTime = sampleTime\n"
		timeoutDetail = `log.Detail("Lyric backfill timed out`
		deferCancel   = "\tdefer cancel()\n"
		fillUp        = "\t\t\t\t\tif !assocKey.hasIDs() {\n"
		claimLine     = "\t\t\tlastName = meta.Name\n\t\t\tlastLineIdx = -1\n"
		// 同一首重开（#218，设计 §6.8「重开」）
		usableLine    = "\t\tloadUsable := mem.Offsets().PlayState.loadingUsable(judge.bad, pollInterval)\n"
		onceLine      = "\t\treopenedThisTick := false\n"
		loadStepCall  = "loadOpen.step(mem.Offsets().PlayState, meta.PlayState, meta.PlayStateOK, sampleTime)"
		loadCondText  = "!reopenedThisTick && reopenByLoading(loadUsable, edge, meta.Name, lastName, assocKey.hasIDs())"
		loadHead      = "\t\tif edge := " + loadStepCall + "; " + loadCondText + " {\n"
		loadIf        = loadHead + "\t\t\treopenedThisTick = true\n\t\t\treopenSong(true)\n\t\t}\n"
		fillLoadIf    = "\t\t\t\t\t\tif edge := " + loadStepCall + "; " + loadCondText + " {\n\t\t\t\t\t\t\treopenedThisTick = true\n\t\t\t\t\t\t\treopenSong(true)\n\t\t\t\t\t\t}\n"
		fillState     = "\t\t\t\t\t\tmeta.PlayState, meta.PlayStateOK = fresh.PlayState, fresh.PlayStateOK\n"
		claimState    = "\t\t\t\t\tmeta.PlayState, meta.PlayStateOK = fresh.PlayState, fresh.PlayStateOK\n"
		fillFerr      = "\t\t\t\t\tif ferr == nil {\n"
		claimLoadIf   = "\t\t\t\t\tif edge := " + loadStepCall + "; " + loadCondText + " {\n\t\t\t\t\t\treopenedThisTick = true\n\t\t\t\t\t\treopenSong(true)\n\t\t\t\t\t}\n"
		heldBlock     = "\t\tif held {\n\t\t\tcontinue\n\t\t}\n"
		holdHead      = "\t\t// --- 换歌前进度归零的暂扣（#202）---"
		holdIf        = "\t\t\t\tif !reopenedThisTick && !loadUsable && released && meta.ProgressMs < lastMemProgress && assocKey.hasIDs() {\n\t\t\t\t\treopenedThisTick = true\n\t\t\t\t\treopenSong(false)\n\t\t\t\t}\n"
		seekIf        = "\t\t\tif seekDetected {\n"
		changeHead    = "\t\t// --- 快速计时器锚点更新 + seek 检测 ---"
		reopenInc     = "\t\tassocKey.reopen++\n"
		reopenedLine  = "\t\tlyricSt.reopened(assocKey.reopen)\n"
		reopenSubmit  = "\t\treader.Submit(assocKey)\n\t\tif byLoading {\n"
		fillZero      = "\t\t\t\t\t\tassocKey.reopen = 0\n"
		fillSubmitUp  = "\t\t\t\t\t\tlyricSt.reset(assocKey.gen)\n\t\t\t\t\t\treader.Submit(assocKey)\n"
		pollLine      = "\tpollInterval := EffectivePollInterval(p.pollMs)\n"
		stepClassify  = "loading := rawOK && spec.classify(raw) == stateLoading"
		judgeClassify = "v.st = in.spec.classify(in.raw)"
		byLoadingRet  = `return usable && edge && lastName != "" && name == lastName && hasIDs`
		// 详情结果的交接（#218 第 4 版，设计 §6.4 第 2 步「交接」）
		claimApply   = "applyDetail(assocKey, meta.SongID, meta.SongMid, det)"
		claimHandoff = "\t\t\t\tif k, ok := " + claimApply + "; ok {\n\t\t\t\t\tassocKey = k\n\t\t\t\t\treader.Submit(assocKey)\n\t\t\t\t}\n"
		fillCover    = "\t\t\t\t\t\tcoverURL, det := fetchCoverURL(sid, smid)\n"
		fillApply    = "applyDetail(assocKey, sid, smid, det)"
		fillHandoff  = "\t\t\t\t\t\tif k, ok := " + fillApply + "; ok {\n\t\t\t\t\t\t\tassocKey = k\n\t\t\t\t\t\t\treader.Submit(assocKey)\n\t\t\t\t\t\t}\n"
		exhaustIf    = "\t\t\t\tif k, ok := exhaustDetail(assocKey); ok {\n\t\t\t\t\tassocKey = k\n\t\t\t\t\treader.Submit(assocKey)\n\t\t\t\t}\n"
		giveUpCase   = "\t\t\tcase backfillGiveUp:\n"
		holdCase     = "\t\t\tcase backfillHold:\n"
		fillClear    = "\t\t\t\t\t\tassocKey.detail = songDetail{}\n"
		fillExhaust  = "\t\t\t\t\t\tif k, ok := exhaustDetail(assocKey); ok {\n\t\t\t\t\t\t\tassocKey = k\n\t\t\t\t\t\t\treader.Submit(assocKey)\n\t\t\t\t\t\t}\n"
		// 改名证据与载入段的记录（#218 第 5 版，设计 §6.8「改名证据」）
		prevLine    = "\t\t\tprevLocal := lyricSt.localPath()\n"
		claimEvLine = "\t\t\trenamed := loadOpen.claim(loadUsable, mem.Offsets().PlayState, pollInterval)\n"
		logEvLine   = "\t\t\tlogRenameEvidence(prevLocal, renamed)\n"
		resetLine   = "\t\t\tlyricSt.reset(assocKey.gen)\n"
		excludeExpr = "excludeLocal: excludeFor(prevLocal, renamed)"
		missedLine  = "\t\t\tloadOpen.missed() // 这一拍没有读到播放状态：改名证据不成立（设计 §6.8「改名证据」）\n"
		newEdgeLine = "\tloadOpen := newLoadEdge(pollInterval)"
	)
	nowStep := func(block string) string { return strings.Replace(block, ", sampleTime)", ", time.Now())", 1) }
	cases := []struct {
		name   string
		mutate func(string) string
		needle string // 改写依赖的原文
	}{
		{"去掉 defer cancel()", replace(deferCancel, ""), deferCancel},
		{"每拍接收改成不带 default（takeResult 去掉 default）", replace(takeDefault, "\t}\n\treturn lines, changed\n"), takeDefault},
		{"takeResult 循环取完", func(s string) string {
			s = strings.Replace(s, "\tselect {\n\tcase res := <-results:", "\tfor len(results) > 0 {\n\tselect {\n\tcase res := <-results:", 1)
			return strings.Replace(s, takeDefault, "\tdefault:\n\t}\n\t}\n\treturn lines, changed\n", 1)
		}, takeDefault},
		{"每拍改为直接接收结果通道", replace(tickHead, "\t\tif lastName != \"\" {\n\t\t\tif res := <-reader.Results(); true {\n\t\t\t\tlines, _ := lyricSt.apply(res, assocKey.hasIDs(), offsetSec)\n"), tickHead},
		{"每拍的实参不看编号", replace(tickHead, strings.Replace(tickHead, "assocKey.hasIDs()", "true", 1)), tickHead},
		{"每拍在认领之前也取", replace(tickHead, strings.Replace(tickHead, `lastName != ""`, "true", 1)), tickHead},
		{"每拍取结果挪到暂停检测之前", move(tickBlock, pauseHead, false), tickBlock},
		{"每拍换词时取封面", replace(tickShow, tickShow+"\t\t\t\t_, _ = fetchCoverURL(assocKey.songID, assocKey.songMid)\n"), tickShow},
		{"每拍换词时结束补取", replace(tickShow, tickShow+"\t\t\t\tlyricsPending = lyricSt.source() != srcLocal\n"), tickShow},
		{"换歌分支 Submit 挪到取词之后", move(claimSubmit, claimCover, true), claimSubmit},
		{"换歌分支 gen 不加一", replace("gen: assocKey.gen + 1,", "gen: assocKey.gen,"), "gen: assocKey.gen + 1,"},
		{"换歌分支 prevLocal 挪到 reset 之后", move(prevLine, resetLine, true), prevLine},
		{"换歌分支取词成功后不取结果", replace(claimTake, ""), claimTake},
		{"换歌分支取词失败也存在线原始行", replace(claimRaw, "\t\t\t\tlyricSt.setOnlineRaw(currentLyrics)\n\t\t\t\tif err == nil {\n"), claimRaw},
		{"换歌分支编号缺位时不登记「无」", replace(claimNoIDs, "\t\t\t} else {\n"), claimNoIDs},
		{"补取块编号由缺到有时 gen 不加一", replace("\t\t\t\t\t\tassocKey.gen++\n", ""), "\t\t\t\t\t\tassocKey.gen++\n"},
		{"补取块 Submit 挪到取词之后", move(fillSubmit, fillFetch, true), fillSubmit},
		{"补取成功只在有变化时发", replace(fillShow, "\t\t\t\t\t\tif changed {\n"+fillShow+"\t\t\t\t\t\t}\n"), fillShow},
		{"补取成功无变化时不以当前显示重发", replace(fillKeep, ""), fillKeep},
		{"补取块绕过 showLyrics 直接发", replace(fillShow, "\t\t\t\t\t\tp.emitNoLyrics(currentTitle, currentDurationSec, 0, 0)\n"), fillShow},
		{"补取块采纳关联即结束补取", replace(fillAdopt, fillAdopt+"\t\t\t\t\t\t\tlyricsPending = false\n"), fillAdopt},
		{"补取超时一律 Warn", replace(timeoutDetail, `log.Warn("Lyric backfill timed out`), timeoutDetail},
		{"showLyrics 删掉 lastLineIdx = -1", replace(showReset, "\t\tcurrentLyrics = lines\n"), showReset},
		{"showLyrics 位置不看暂停", replace(showPos, strings.Replace(showPos, "!isPaused", "true", 1)), showPos},
		{"showLyrics 不再冻结", replace(showFreeze, ""), showFreeze},
		{"showLyrics 只在有歌词一支冻结", move(showFreeze, showLyricsEnd, true), showFreeze},
		{"showLyrics 冻结在锚点而非 all_lyrics 的位置", replace("\t\t\trepausePos = pos\n", "\t\t\trepausePos = float32(anchorProgressMs) / 1000.0\n"), "\t\t\trepausePos = pos\n"},
		{"showLyrics 补发入队失败不记", replace(showRecord, "\t\t\tp.emitFrozenPause(repausePos, currentDurationSec)\n\t\t}\n\t}\n"), showRecord},
		{"emitFrozenPause 改用会静默丢弃的 Emit", replace(frozenTry, "p.Emit(player.EventPlaybackPause,"), frozenTry},
		{"入队失败不再试", replace(retryBlock, ""), retryBlock},
		{"再试不看是否仍在暂停", replace(retryBlock, strings.Replace(retryBlock, "repausePending && isPaused", "repausePending", 1)), retryBlock},
		{"再试改用锚点位置", replace(retryBlock, strings.Replace(retryBlock, "emitFrozenPause(repausePos,", "emitFrozenPause(float32(anchorProgressMs)/1000.0,", 1)), retryBlock},
		{"再试挪到暂停检测之前", move(retryBlock, pauseHead, false), retryBlock},
		{"换歌时不清待补发", replace(claimClear, "\t\t\tisPaused = false\n\t\t\tjudge.resume.reset()\n"), claimClear},
		{"恢复时不清待补发", replace(resumeClear, "\t\t\tisPaused = false\n\t\t\tanchorTime = sampleTime\n"), resumeClear},
		{"未有结论时显示为空也发", replace(showEarly, ""), showEarly},
		{"未有结论时也发 index:-1", replace(showNoLyrIf, "if len(lines) == 0 {"), showNoLyrIf},
		{"未有结论不看补取", replace(showUndecided, "lyricsUndecided(lines, false, lyricSt.source())"), showUndecided},
		{"showLyrics 无歌词一支的位置不取 pos", replace(showNoLyrics, strings.Replace(showNoLyrics, "pos, prog", "0, prog", 1)), showNoLyrics},
		{"showLyrics 里取封面", replace(showReset, showReset+"\t\t_, _ = fetchCoverURL(assocKey.songID, assocKey.songMid)\n"), showReset},
		{"takeResult 不经 apply", replace("st.apply(res, songHasIDs, offsetSec)", "st.refresh(songHasIDs, offsetSec)"), "st.apply(res, songHasIDs, offsetSec)"},
		{"补取块每次重试都加 gen", replace(fillUp, "\t\t\t\t\tif true {\n"), fillUp},
		{"换歌分支不重置行号", replace(claimLine, "\t\t\tlastName = meta.Name\n"), claimLine},
		// 读当前歌曲记录只在读取者里
		{"runSession 直接扫当前歌曲记录", replace(pollLine, "\t_, _, _ = scanRecord(memSource{}, nil, recordScanLimits, nil)\n"+pollLine), pollLine},
		{"runSession 直接读 Struct1 的前缀", replace(pollLine, "\t_, _, _, _ = readSongPrefix(memSource{}, 0, mem.Offsets())\n"+pollLine), pollLine},
		// 同一首重开：载入支
		{"载入那处挪到 hold.step 之后", move(loadIf, heldBlock, true), loadIf},
		{"载入那处挪到换歌检测之后、暂扣之前", move(loadIf, holdHead, false), loadIf},
		{"载入支去掉歌名相等（实参）", replace("edge, meta.Name, lastName,", "edge, lastName, lastName,"), "edge, meta.Name, lastName,"},
		{"载入支去掉歌名相等（函数体）", replace(byLoadingRet, `return usable && edge && lastName != "" && hasIDs`), byLoadingRet},
		{"载入支去掉编号已知（实参）", replace(loadIf, strings.Replace(loadIf, "assocKey.hasIDs()", "true", 1)), loadIf},
		{"载入支去掉编号已知（函数体）", replace(byLoadingRet, `return usable && edge && lastName != "" && name == lastName`), byLoadingRet},
		{"载入支去掉 loadUsable", replace(loadIf, strings.Replace(loadIf, "reopenByLoading(loadUsable,", "reopenByLoading(true,", 1)), loadIf},
		{"两处都去掉 loadUsable", func(s string) string {
			s = strings.Replace(s, loadIf, strings.Replace(loadIf, "reopenByLoading(loadUsable,", "reopenByLoading(true,", 1), 1)
			return strings.Replace(s, "if !reopenedThisTick && !loadUsable && released &&", "if !reopenedThisTick && released &&", 1)
		}, loadIf},
		{"载入支自写取值而不经 step", replace(loadHead, strings.Replace(loadHead, loadStepCall, "meta.PlayStateOK && meta.PlayState == 2", 1)), loadHead},
		{"载入支的条件里自写取值", replace(loadHead, strings.Replace(loadHead, "reopenByLoading(loadUsable, edge,", "reopenByLoading(loadUsable, meta.PlayState == 2,", 1)), loadHead},
		{"loadOpen.step 只在载入信号可用时调用", replace(loadIf, "\t\tif loadUsable {\n"+loadIf+"\t\t}\n"), loadIf},
		// 同一首重开：取词之后的重读（终审 F1）
		{"补取重读处漏调 loadOpen.step", replace(fillLoadIf, ""), fillLoadIf},
		{"补取重读处只 step、不写重开", replace(fillLoadIf, "\t\t\t\t\t\t"+loadStepCall+"\n"), fillLoadIf},
		{"补取重读的载入支去掉 loadUsable", replace(fillLoadIf, strings.Replace(fillLoadIf, "reopenByLoading(loadUsable,", "reopenByLoading(true,", 1)), fillLoadIf},
		{"补取重读的载入支挪到换上播放状态之前", move(fillLoadIf, fillState, false), fillLoadIf},
		{"补取重读的载入支挪出采用的分支", move(fillLoadIf, fillFerr, false), fillLoadIf},
		{"换歌重读处漏调 loadOpen.step", replace(claimLoadIf, ""), claimLoadIf},
		{"换歌重读处只 step、不写重开", replace(claimLoadIf, "\t\t\t\t\t"+loadStepCall+"\n"), claimLoadIf},
		{"换歌重读的载入支去掉 loadUsable", replace(claimLoadIf, strings.Replace(claimLoadIf, "reopenByLoading(loadUsable,", "reopenByLoading(true,", 1)), claimLoadIf},
		{"换歌重读的载入支挪到换上播放状态之前", move(claimLoadIf, claimState, false), claimLoadIf},
		// 同一首重开：每拍至多写入一次
		{"去掉本拍一次的约束（拍首载入支）", replace(loadIf, strings.Replace(loadIf, "!reopenedThisTick && ", "", 1)), loadIf},
		{"去掉本拍一次的约束（补取重读的载入支）", replace(fillLoadIf, strings.Replace(fillLoadIf, "!reopenedThisTick && ", "", 1)), fillLoadIf},
		{"去掉本拍一次的约束（换歌重读的载入支）", replace(claimLoadIf, strings.Replace(claimLoadIf, "!reopenedThisTick && ", "", 1)), claimLoadIf},
		{"去掉 reopenedThisTick（声明、条件与置真全删）", func(s string) string {
			s = strings.Replace(s, onceLine, "", 1)
			s = strings.ReplaceAll(s, "!reopenedThisTick && ", "")
			for _, ind := range []string{"\t\t\t", "\t\t\t\t\t", "\t\t\t\t\t\t", "\t\t\t\t\t\t\t"} {
				s = strings.ReplaceAll(s, ind+"reopenedThisTick = true\n", "")
			}
			return s
		}, onceLine},
		{"去掉本拍一次的约束（暂扣支）", replace(holdIf, strings.Replace(holdIf, "!reopenedThisTick && ", "", 1)), holdIf},
		{"补取重读写入后不记本拍已写", replace(fillLoadIf, strings.Replace(fillLoadIf, "\t\t\t\t\t\t\treopenedThisTick = true\n", "", 1)), fillLoadIf},
		{"拍中途清掉本拍已写", replace(changeHead, "\t\treopenedThisTick = false\n"+changeHead), changeHead},
		{"不声明本拍已写、改用循环外的变量", replace(onceLine, ""), onceLine},
		{"loadEdge.step 不经 classify", replace(stepClassify, "loading := rawOK && raw == 2"), stepClassify},
		{"judgeTick 不经 classify", replace(judgeClassify, "v.st = classifyRaw(in.spec, in.raw)"), judgeClassify},
		{"loadUsable 不看本会话是否判不可信", replace(usableLine, strings.Replace(usableLine, "judge.bad", "false", 1)), usableLine},
		{"变化分支之前重算 loadUsable", replace(changeHead, "\t\tloadUsable = len(mem.Offsets().PlayState.Loading) > 0\n"+changeHead), changeHead},
		// 同一首重开：暂扣支
		{"暂扣支去掉 !loadUsable", replace(holdIf, strings.Replace(holdIf, "!loadUsable && ", "", 1)), holdIf},
		{"暂扣支去掉 released", replace(holdIf, strings.Replace(holdIf, "released && ", "", 1)), holdIf},
		{"暂扣支另算载入信号", replace(holdIf, strings.Replace(holdIf, "!loadUsable &&", "!mem.Offsets().PlayState.loadingUsable(judge.bad, pollInterval) &&", 1)), holdIf},
		{"暂扣支挪出 if seekDetected", move(holdIf, seekIf, false), holdIf},
		// 同一首重开：reopenSong
		{"去掉 reopen++", replace(reopenInc, ""), reopenInc},
		{"reopen++ 改为 gen++", replace(reopenInc, "\t\tassocKey.gen++\n"), reopenInc},
		{"闭包里加一次 reset", replace(reopenedLine, reopenedLine+"\t\tlyricSt.reset(assocKey.gen)\n"), reopenedLine},
		{"闭包里以 reset 代替 reopened", replace(reopenedLine, "\t\tlyricSt.reset(assocKey.gen)\n"), reopenedLine},
		{"漏掉 reopened", replace(reopenedLine, ""), reopenedLine},
		{"漏掉写入读取者", replace(reopenSubmit, "\t\tif byLoading {\n"), reopenSubmit},
		{"写入挪到 reopened 之前", replace(reopenedLine+reopenSubmit, "\t\treader.Submit(assocKey)\n"+reopenedLine+"\t\tif byLoading {\n"), reopenedLine + reopenSubmit},
		{"reopenSong 多一处调用", replace(claimSubmit, claimSubmit+"\t\t\treopenSong(true)\n"), claimSubmit},
		// gen 加一时 reopen 归零
		{"补取块漏掉 reopen = 0", replace(fillZero, ""), fillZero},
		{"补取块 reopen = 0 挪到 Submit 之后", move(fillZero, fillSubmitUp, true), fillZero},
		{"换歌分支的 songKey 沿用 reopen", replace("songKey{gen: assocKey.gen + 1, ", "songKey{gen: assocKey.gen + 1, reopen: assocKey.reopen, "), "songKey{gen: assocKey.gen + 1, "},
		// 详情结果的交接（#218 第 4 版）
		{"去掉换歌分支的详情写入", replace(claimHandoff, ""), claimHandoff},
		{"去掉补取块的详情写入", replace(fillHandoff, ""), fillHandoff},
		{"换歌分支的详情写入挪到 fetchCoverURL 之前", move(claimHandoff, claimCover, false), claimHandoff},
		{"补取块的详情写入挪到 fetchCoverURL 之前", move(fillHandoff, fillCover, false), fillHandoff},
		{"换歌分支 applyDetail 的所查编号换成 assocKey 自己的", replace(claimApply, "applyDetail(assocKey, assocKey.songID, assocKey.songMid, det)"), claimApply},
		{"补取块 applyDetail 的所查编号换成 assocKey 自己的", replace(fillApply, "applyDetail(assocKey, assocKey.songID, assocKey.songMid, det)"), fillApply},
		{"换歌分支去掉 if、每次都写入", replace(claimHandoff, "\t\t\t\tassocKey, _ = "+claimApply+"\n\t\t\t\treader.Submit(assocKey)\n"), claimHandoff},
		{"补取块去掉 if、每次都写入", replace(fillHandoff, "\t\t\t\t\t\tassocKey, _ = "+fillApply+"\n\t\t\t\t\t\treader.Submit(assocKey)\n"), fillHandoff},
		{"补取块的 fetchCoverURL 挪到 showLyrics 之前", move(fillCover+fillHandoff, fillShow, false), fillCover + fillHandoff},
		{"漏掉 case backfillGiveUp 的写入", replaceAfter(giveUpCase, exhaustIf, ""), giveUpCase},
		{"漏掉 case backfillHold 的写入", replaceAfter(holdCase, exhaustIf, ""), holdCase},
		{"backfillHold 里不经 exhaustDetail 直接写已失败", replaceAfter(holdCase, exhaustIf,
			"\t\t\t\tassocKey.detail = songDetail{state: detailFailed}\n\t\t\t\treader.Submit(assocKey)\n"), holdCase},
		{"为补全另加一次 fetchCoverURL", replace(fillFetch, "\t\t\t\t\tif _, d := fetchCoverURL(sid, smid); d.state == detailGot {\n"+
			"\t\t\t\t\t\tassocKey.detail = d\n\t\t\t\t\t}\n"+fillFetch), fillFetch},
		{"每拍取结果时调用 fetchSongMidByID", replace(tickShow, tickShow+"\t\t\t\t_, _ = fetchSongMidByID(assocKey.songID)\n"), tickShow},
		{"补取块编号由缺到有时不清详情结果", replace(fillClear, ""), fillClear},
		// 补取换了所查编号而衔接不上（终审第 4 版第 2 条）
		{"漏掉补取成功之后的 exhaustDetail", replace(fillExhaust, ""), fillExhaust},
		{"补取成功之后的 exhaustDetail 挪到 applyDetail 之前", move(fillExhaust, fillHandoff, false), fillExhaust},
		{"补取成功之后的 exhaustDetail 挪出取词成功的分支", move(fillExhaust, "\t\t\t\t// 只有重试满 lyricRetryLogAfter 次仍没取到才打一行", false), fillExhaust},
		{"补取成功之后不经 exhaustDetail 直接写已失败", replace(fillExhaust,
			"\t\t\t\t\t\tif assocKey.detail.state == detailNone {\n\t\t\t\t\t\t\tassocKey.detail = songDetail{state: detailFailed}\n\t\t\t\t\t\t\treader.Submit(assocKey)\n\t\t\t\t\t\t}\n"), fillExhaust},
		{"补取成功之后去掉 if、每次都写入", replace(fillExhaust, "\t\t\t\t\t\tassocKey, _ = exhaustDetail(assocKey)\n\t\t\t\t\t\treader.Submit(assocKey)\n"), fillExhaust},
		// 改名证据与载入段的记录（#218 第 5 版）
		{"claim 的 loadUsable 换成 true", replace(claimEvLine, strings.Replace(claimEvLine, "claim(loadUsable,", "claim(true,", 1)), claimEvLine},
		{"claim 的 pollInterval 换成 0", replace(claimEvLine, strings.Replace(claimEvLine, ", pollInterval)", ", 0)", 1)), claimEvLine},
		{"claim 挪进补取块编号由缺到有的那一支", move(claimEvLine, fillSubmitUp, false), claimEvLine},
		{"claim 挪进 reopenSong 闭包", move(claimEvLine, reopenedLine, false), claimEvLine},
		{"换歌分支调两次 claim", replace(claimEvLine, claimEvLine+strings.Replace(claimEvLine, ":=", "=", 1)), claimEvLine},
		{"excludeLocal 写成 lyricSt.localPath()", replace(claimKey, strings.Replace(claimKey, excludeExpr, "excludeLocal: lyricSt.localPath()", 1)), claimKey},
		{"excludeLocal 写成空串", replace(claimKey, strings.Replace(claimKey, excludeExpr, `excludeLocal: ""`, 1)), claimKey},
		{"去掉 logRenameEvidence", replace(logEvLine, ""), logEvLine},
		{"元数据读失败的分支去掉 missed", replace(missedLine, ""), missedLine},
		{"missed 挪到 continue 之后", replace(missedLine+"\t\t\tcontinue\n", "\t\t\tcontinue\n"+missedLine), missedLine + "\t\t\tcontinue\n"},
		{"另建一个 loadEdge 实例求证据", replace(claimEvLine, "\t\t\tvar lw loadEdge\n"+strings.Replace(claimEvLine, "loadOpen.claim(", "lw.claim(", 1)), claimEvLine},
		{"拍首的 step 改传 time.Now()", replace(loadIf, nowStep(loadIf)), loadIf},
		{"换歌重读的 step 改传 time.Now()", replace(claimLoadIf, nowStep(claimLoadIf)), claimLoadIf},
		{"补取重读的 step 改传 time.Now()", replace(fillLoadIf, nowStep(fillLoadIf)), fillLoadIf},
		{"loadOpen 改回 var loadOpen loadEdge", replace(newEdgeLine, "\tvar loadOpen loadEdge"), newEdgeLine},
		{"补取块编号由缺到有时清掉 excludeLocal", replace(fillClear, fillClear+"\t\t\t\t\t\tassocKey.excludeLocal = \"\"\n"), fillClear},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(src, c.needle) {
				t.Fatalf("qqmusic.go 里找不到改写依赖的原文 %q —— 本条自测失效", c.needle)
			}
			mutated := c.mutate(src)
			if mutated == src {
				t.Fatalf("改写没有生效 —— 本条自测失效")
			}
			real := 0 // 报出的接线问题，不算解析失败与门禁失效
			probs := assocWiringProblems(mutated)
			for _, p := range probs {
				if !strings.HasPrefix(p, "解析失败") && !strings.Contains(p, "门禁失效") {
					real++
				}
			}
			if real == 0 {
				t.Fatalf("门禁没有报出这处改写的接线问题：%q", probs)
			}
		})
	}
}
