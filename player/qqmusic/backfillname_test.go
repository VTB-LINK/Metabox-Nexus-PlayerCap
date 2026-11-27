package qqmusic

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

type qqSessionAnchors struct {
	fset          *token.FileSet
	run           *ast.FuncDecl
	show          *ast.FuncLit
	backfillCover *ast.FuncLit
	sw            *ast.IfStmt
	fill          *ast.IfStmt
	retry         *ast.CaseClause
	success       *ast.IfStmt
}

func findQQSessionAnchors(src string) (*qqSessionAnchors, string) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "qqmusic.go", src, 0)
	if err != nil {
		return nil, "解析失败: " + err.Error()
	}
	str := types.ExprString
	var run *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && fd.Name.Name == "runSession" {
			run = fd
		}
	}
	if run == nil {
		return nil, "没找到 runSession —— 门禁失效"
	}
	var show, backfillCover *ast.FuncLit
	var sw, fill *ast.IfStmt
	for _, s := range run.Body.List {
		switch s := s.(type) {
		case *ast.AssignStmt:
			if len(s.Lhs) == 1 && len(s.Rhs) == 1 {
				if fl, ok := s.Rhs[0].(*ast.FuncLit); ok {
					switch str(s.Lhs[0]) {
					case "showLyrics":
						show = fl
					case "runBackfillCover":
						backfillCover = fl
					}
				}
			}
		case *ast.ForStmt:
			for _, st := range s.Body.List {
				ifs, ok := st.(*ast.IfStmt)
				switch {
				case !ok:
				case sw == nil && strings.HasPrefix(str(ifs.Cond), "meta.Name != lastName"):
					sw = ifs
				case fill == nil && strings.Contains(str(ifs.Cond), "lyricsPending"):
					fill = ifs
				}
			}
		}
	}
	if show == nil || backfillCover == nil || sw == nil || fill == nil {
		return nil, "没找到 showLyrics、runBackfillCover、换歌分支或补取块 —— 门禁失效"
	}
	var retry *ast.CaseClause
	ast.Inspect(fill.Body, func(x ast.Node) bool {
		if cc, ok := x.(*ast.CaseClause); ok && len(cc.List) == 1 && str(cc.List[0]) == "backfillRetry" {
			retry = cc
		}
		return true
	})
	if retry == nil {
		return nil, "没找到 case backfillRetry —— 门禁失效"
	}
	var success *ast.IfStmt
	ast.Inspect(retry, func(x ast.Node) bool {
		if ifs, ok := x.(*ast.IfStmt); ok && str(ifs.Cond) == "ferr == nil" {
			success = ifs
		}
		return true
	})
	if success == nil {
		return nil, "没找到取词成功分支 if ferr == nil —— 门禁失效"
	}
	return &qqSessionAnchors{
		fset:          fset,
		run:           run,
		show:          show,
		backfillCover: backfillCover,
		sw:            sw,
		fill:          fill,
		retry:         retry,
		success:       success,
	}, ""
}

// isBackfillNamesAssign 判断语句是否为补取块内唯一允许的歌名/歌手更新写法：
// currentName, currentSinger = backfillNames(currentName, currentSinger, meta.Name, meta.Singer, lrcName, lrcSinger)。
func isBackfillNamesAssign(a *ast.AssignStmt) bool {
	str := types.ExprString
	if a.Tok != token.ASSIGN || len(a.Lhs) != 2 || len(a.Rhs) != 1 {
		return false
	}
	if str(a.Lhs[0]) != "currentName" || str(a.Lhs[1]) != "currentSinger" {
		return false
	}
	return str(a.Rhs[0]) == "backfillNames(currentName, currentSinger, meta.Name, meta.Singer, lrcName, lrcSinger)"
}

func isCoverURLFallbackIf(s ast.Stmt) bool {
	str := types.ExprString
	ifs, ok := s.(*ast.IfStmt)
	if !ok || ifs.Init != nil || ifs.Else != nil || str(ifs.Cond) != `coverURL == ""` || len(ifs.Body.List) != 1 {
		return false
	}
	a, ok := ifs.Body.List[0].(*ast.AssignStmt)
	return ok && a.Tok == token.ASSIGN && len(a.Lhs) == 1 && len(a.Rhs) == 1 &&
		str(a.Lhs[0]) == "coverURL" && str(a.Rhs[0]) == "currentCoverURL"
}

func isNewSongGenDefine(s ast.Stmt) (string, bool) {
	str := types.ExprString
	a, ok := s.(*ast.AssignStmt)
	if !ok || a.Tok != token.DEFINE || len(a.Lhs) != 1 || len(a.Rhs) != 1 {
		return "", false
	}
	if str(a.Rhs[0]) != "p.NewSongGen()" {
		return "", false
	}
	id, ok := a.Lhs[0].(*ast.Ident)
	if !ok || id.Name == "_" {
		return "", false
	}
	return id.Name, true
}

func songInfoFieldValues(c *ast.CallExpr) (map[string]string, bool) {
	str := types.ExprString
	if len(c.Args) != 2 {
		return nil, false
	}
	u, ok := c.Args[1].(*ast.UnaryExpr)
	if !ok || u.Op != token.AND {
		return nil, false
	}
	cl, ok := u.X.(*ast.CompositeLit)
	if !ok || str(cl.Type) != "player.SongInfo" {
		return nil, false
	}
	fields := map[string]string{}
	for _, el := range cl.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			fields[str(kv.Key)] = str(kv.Value)
		}
	}
	return fields, true
}

func flatParamNames(fl *ast.FuncLit) []string {
	if fl == nil || fl.Type == nil || fl.Type.Params == nil {
		return nil
	}
	var out []string
	for _, f := range fl.Type.Params.List {
		for _, n := range f.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

// backfillNameProblems 检查 src（qqmusic.go 的源码）里 runSession 认领分支、补取块与 runBackfillCover 的 #229 约束（Q-G）：
//   - 补取块：fetchLRC 接收 lrcName、lrcSinger；取词成功分支经 backfillNames 更新 currentName、currentSinger，
//     不得给 currentTitle 赋值，不得绕过 backfillNames 赋值 currentName/currentSinger；发出元数据前取
//     name, singer, title := currentName, currentSinger, currentTitle；
//   - 补取代次与封面回退（含 #248 延后分流）：取词成功分支顶层先声明 coverGen := p.NewSongGen()（早于第一处 p.Emit 与封面分流），
//     pendingCover.schedule 与立即执行的 runBackfillCover 均透传该 coverGen 与 (sid, smid, name, singer, title)，延后排空处透传 pc.coverGen；
//     runBackfillCover 内不得另调 p.NewSongGen() 或改写代次/名称形参，并在 coverURL, det := fetchCoverURL(sid, smid) 之后、
//     p.startCover 之前有 if coverURL == "" { coverURL = currentCoverURL }，以形参代次调 p.startCover；
//   - 认领分支：有 currentName, currentSinger = meta.Name, meta.Singer、currentCoverURL = coverURL，
//     有本分支声明的 coverGen := p.NewSongGen()（早于首条宣告事件）并以此代次调 p.startCover。
func backfillNameProblems(src string) []string {
	a, fail := findQQSessionAnchors(src)
	if fail != "" {
		return []string{fail}
	}
	str := types.ExprString
	within := func(n, b ast.Node) bool { return n.Pos() >= b.Pos() && n.End() <= b.End() }
	var probs []string

	// ---- 1. 认领分支 sw.Body ----
	hasClaimNames := false
	hasClaimCoverURL := false
	var claimGenVar string
	var claimGenPos token.Pos
	var claimGenTopCount int
	var claimGenTotal int
	var claimFirstEmitPos token.Pos
	var claimStartCalls []*ast.CallExpr
	for _, st := range a.sw.Body.List {
		if name, ok := isNewSongGenDefine(st); ok {
			claimGenTopCount++
			claimGenVar = name
			claimGenPos = st.Pos()
		}
	}
	ast.Inspect(a.sw.Body, func(x ast.Node) bool {
		switch x := x.(type) {
		case *ast.AssignStmt:
			if x.Tok == token.ASSIGN && len(x.Lhs) == 2 && len(x.Rhs) == 2 &&
				str(x.Lhs[0]) == "currentName" && str(x.Lhs[1]) == "currentSinger" &&
				str(x.Rhs[0]) == "meta.Name" && str(x.Rhs[1]) == "meta.Singer" {
				hasClaimNames = true
			}
			if x.Tok == token.ASSIGN && len(x.Lhs) == 1 && len(x.Rhs) == 1 &&
				str(x.Lhs[0]) == "currentCoverURL" && str(x.Rhs[0]) == "coverURL" {
				hasClaimCoverURL = true
			}
			if claimGenVar != "" && x.Pos() > claimGenPos {
				for _, l := range x.Lhs {
					if str(l) == claimGenVar {
						probs = append(probs, "换歌分支在 NewSongGen 之后不得改写 "+claimGenVar)
					}
				}
			}
		case *ast.CallExpr:
			switch str(x.Fun) {
			case "p.NewSongGen":
				claimGenTotal++
			case "p.Emit", "p.emitNoLyrics":
				if claimFirstEmitPos == token.NoPos || x.Pos() < claimFirstEmitPos {
					claimFirstEmitPos = x.Pos()
				}
			case "p.startCover":
				claimStartCalls = append(claimStartCalls, x)
			case "player.FetchCoverBase64":
				probs = append(probs, "换歌分支不应直接调用 player.FetchCoverBase64，应统一经 p.startCover")
			}
		}
		return true
	})
	if !hasClaimNames {
		probs = append(probs, "换歌分支缺少 currentName, currentSinger = meta.Name, meta.Singer")
	}
	if !hasClaimCoverURL {
		probs = append(probs, "换歌分支缺少 currentCoverURL = coverURL")
	}
	if claimGenTopCount != 1 || claimGenTotal != 1 {
		probs = append(probs, fmt.Sprintf("换歌分支顶层应恰有 1 处 <gen> := p.NewSongGen()，实得顶层 %d / 总计 %d", claimGenTopCount, claimGenTotal))
	} else if claimFirstEmitPos == token.NoPos || claimGenPos >= claimFirstEmitPos {
		probs = append(probs, "换歌分支的 NewSongGen 应严格早于首条宣告事件（p.Emit / p.emitNoLyrics）")
	}
	if len(claimStartCalls) != 1 {
		probs = append(probs, fmt.Sprintf("换歌分支应恰有 1 处 p.startCover 调用，实得 %d", len(claimStartCalls)))
	} else {
		c := claimStartCalls[0]
		if claimGenPos != token.NoPos && claimGenPos >= c.Pos() {
			probs = append(probs, "换歌分支的 NewSongGen 应早于 p.startCover")
		}
		if len(c.Args) != 6 || str(c.Args[0]) != "realQQCoverOps" || str(c.Args[1]) != claimGenVar ||
			str(c.Args[2]) != "coverURL" ||
			(str(c.Args[3]) != "currentName" && str(c.Args[3]) != "meta.Name") ||
			(str(c.Args[4]) != "currentSinger" && str(c.Args[4]) != "meta.Singer") ||
			str(c.Args[5]) != "title" {
			probs = append(probs, fmt.Sprintf("换歌分支 p.startCover 实参不符，实得 %s", str(c)))
		}
	}

	// ---- 2. 补取块 fill.Body / retry / success 与 runBackfillCover ----
	hasFetchLRCWithNames := false
	ast.Inspect(a.retry, func(x ast.Node) bool {
		if as, ok := x.(*ast.AssignStmt); ok && len(as.Lhs) == 4 && len(as.Rhs) == 1 &&
			str(as.Lhs[0]) == "lines" && str(as.Lhs[1]) == "lrcName" && str(as.Lhs[2]) == "lrcSinger" && str(as.Lhs[3]) == "ferr" &&
			strings.HasPrefix(str(as.Rhs[0]), "fetchLRC(") {
			hasFetchLRCWithNames = true
		}
		return true
	})
	if !hasFetchLRCWithNames {
		probs = append(probs, "补取块应以 lines, lrcName, lrcSinger, ferr := fetchLRC(...) 接收歌词标签")
	}

	backfillAssignCount := 0
	var backfillAssignPos token.Pos
	for _, scope := range []ast.Node{a.fill.Body, a.backfillCover.Body} {
		ast.Inspect(scope, func(x ast.Node) bool {
			switch x := x.(type) {
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					switch str(l) {
					case "currentTitle":
						probs = append(probs, "补取块与 runBackfillCover 不得给 currentTitle 赋值")
					case "currentName", "currentSinger":
						if !within(x, a.success.Body) || !isBackfillNamesAssign(x) {
							probs = append(probs, "补取块绕过 backfillNames 给 "+str(l)+" 赋值")
						}
					}
				}
				if scope == a.fill.Body && within(x, a.success.Body) && isBackfillNamesAssign(x) {
					backfillAssignCount++
					backfillAssignPos = x.Pos()
				}
			case *ast.CallExpr:
				if str(x.Fun) == "player.FetchCoverBase64" {
					probs = append(probs, "补取路径不应直接调用 player.FetchCoverBase64，应统一经 p.startCover")
				}
			}
			return true
		})
	}
	if backfillAssignCount != 1 {
		probs = append(probs, fmt.Sprintf("补取取词成功分支应恰有 1 处 currentName, currentSinger = backfillNames(currentName, currentSinger, meta.Name, meta.Singer, lrcName, lrcSinger)，实得 %d", backfillAssignCount))
	}

	// 检查 success.Body 顶层语句次序与关键调用
	var (
		fillGenVar       string
		fillGenPos       token.Pos
		fillGenCount     int
		namesBindPos     token.Pos
		namesBindCount   int
		firstEmitPos     token.Pos
		coverDispatchPos token.Pos
		songInfoEmits    []*ast.CallExpr
		schedCalls       []*ast.CallExpr
		directCoverCalls []*ast.CallExpr
	)
	for _, st := range a.success.Body.List {
		if name, ok := isNewSongGenDefine(st); ok {
			fillGenCount++
			fillGenVar = name
			fillGenPos = st.Pos()
		}
		if as, ok := st.(*ast.AssignStmt); ok {
			if as.Tok == token.DEFINE && len(as.Lhs) == 3 && len(as.Rhs) == 3 &&
				str(as.Lhs[0]) == "name" && str(as.Lhs[1]) == "singer" && str(as.Lhs[2]) == "title" &&
				str(as.Rhs[0]) == "currentName" && str(as.Rhs[1]) == "currentSinger" && str(as.Rhs[2]) == "currentTitle" {
				namesBindCount++
				namesBindPos = as.Pos()
			}
		}
	}
	ast.Inspect(a.success.Body, func(x ast.Node) bool {
		switch x := x.(type) {
		case *ast.CallExpr:
			switch str(x.Fun) {
			case "p.Emit":
				if firstEmitPos == token.NoPos || x.Pos() < firstEmitPos {
					firstEmitPos = x.Pos()
				}
				if len(x.Args) == 2 && str(x.Args[0]) == "player.EventSongInfoUpdate" {
					songInfoEmits = append(songInfoEmits, x)
				}
			case "pendingCover.schedule":
				schedCalls = append(schedCalls, x)
				if coverDispatchPos == token.NoPos || x.Pos() < coverDispatchPos {
					coverDispatchPos = x.Pos()
				}
			case "runBackfillCover":
				directCoverCalls = append(directCoverCalls, x)
				if coverDispatchPos == token.NoPos || x.Pos() < coverDispatchPos {
					coverDispatchPos = x.Pos()
				}
			}
		case *ast.AssignStmt:
			if fillGenVar != "" && x.Pos() > fillGenPos {
				for _, l := range x.Lhs {
					if str(l) == fillGenVar {
						probs = append(probs, "补取取词成功分支在 NewSongGen 之后不得改写 "+fillGenVar)
					}
				}
			}
		}
		return true
	})

	totalFillNewGen := 0
	for _, scope := range []ast.Node{a.success.Body, a.backfillCover.Body} {
		ast.Inspect(scope, func(x ast.Node) bool {
			if c, ok := x.(*ast.CallExpr); ok && str(c.Fun) == "p.NewSongGen" {
				totalFillNewGen++
			}
			return true
		})
	}
	if fillGenCount != 1 || totalFillNewGen != 1 {
		probs = append(probs, fmt.Sprintf("补取取词成功分支顶层应恰有 1 处 <gen> := p.NewSongGen() 且 runBackfillCover 内不得另调，实得顶层 %d / 总计 %d", fillGenCount, totalFillNewGen))
	}
	if namesBindCount != 1 {
		probs = append(probs, fmt.Sprintf("补取取词成功分支应恰有 1 处 name, singer, title := currentName, currentSinger, currentTitle，实得 %d", namesBindCount))
	} else {
		if backfillAssignPos != token.NoPos && namesBindPos <= backfillAssignPos {
			probs = append(probs, "name, singer, title := currentName, currentSinger, currentTitle 应在 backfillNames 赋值之后")
		}
		if firstEmitPos != token.NoPos && namesBindPos >= firstEmitPos {
			probs = append(probs, "name, singer, title := currentName, currentSinger, currentTitle 应在补取第一处 p.Emit 之前")
		}
	}
	if len(songInfoEmits) != 1 {
		probs = append(probs, fmt.Sprintf("补取取词成功分支应恰有 1 处 p.Emit(player.EventSongInfoUpdate, ...)，实得 %d", len(songInfoEmits)))
	} else {
		fields, ok := songInfoFieldValues(songInfoEmits[0])
		if !ok || fields["Name"] != "name" || fields["Singer"] != "singer" || fields["Title"] != "title" {
			probs = append(probs, fmt.Sprintf("补取发出的 SongInfo 应使用 Name: name, Singer: singer, Title: title，实得 %v", fields))
		}
	}
	if fillGenPos != token.NoPos {
		if firstEmitPos == token.NoPos || fillGenPos >= firstEmitPos {
			probs = append(probs, "补取取词成功分支的 NewSongGen 应早于块内第一处 p.Emit")
		}
		if coverDispatchPos == token.NoPos || fillGenPos >= coverDispatchPos {
			probs = append(probs, "补取取词成功分支的 NewSongGen 应早于封面分流（pendingCover.schedule / runBackfillCover）")
		}
	}
	if len(schedCalls) != 1 {
		probs = append(probs, fmt.Sprintf("补取取词成功分支应恰有 1 处 pendingCover.schedule 调用，实得 %d", len(schedCalls)))
	} else if c := schedCalls[0]; len(c.Args) != 8 || str(c.Args[2]) != fillGenVar ||
		str(c.Args[5]) != "name" || str(c.Args[6]) != "singer" || str(c.Args[7]) != "title" {
		probs = append(probs, fmt.Sprintf("补取 pendingCover.schedule 应透传代次 %q 与 (name, singer, title)，实得 %s", fillGenVar, str(c)))
	}
	if len(directCoverCalls) != 1 {
		probs = append(probs, fmt.Sprintf("补取取词成功分支应恰有 1 处 runBackfillCover 调用，实得 %d", len(directCoverCalls)))
	} else if c := directCoverCalls[0]; len(c.Args) != 6 || str(c.Args[0]) != fillGenVar ||
		str(c.Args[3]) != "name" || str(c.Args[4]) != "singer" || str(c.Args[5]) != "title" {
		probs = append(probs, fmt.Sprintf("补取 runBackfillCover 应透传代次 %q 与 (name, singer, title)，实得 %s", fillGenVar, str(c)))
	}

	// 检查延后排空处 runBackfillCover 调用透传 pc.coverGen 与 pc.name/pc.singer/pc.title
	var allCoverCalls []*ast.CallExpr
	ast.Inspect(a.run.Body, func(x ast.Node) bool {
		if c, ok := x.(*ast.CallExpr); ok && str(c.Fun) == "runBackfillCover" {
			allCoverCalls = append(allCoverCalls, c)
		}
		return true
	})
	if len(allCoverCalls) != 2 {
		probs = append(probs, fmt.Sprintf("runSession 里应恰有 2 处 runBackfillCover 调用（补取与延后排空），实得 %d", len(allCoverCalls)))
	} else {
		for _, c := range allCoverCalls {
			if within(c, a.success.Body) {
				continue
			}
			if len(c.Args) != 6 || str(c.Args[0]) != "pc.coverGen" ||
				str(c.Args[3]) != "pc.name" || str(c.Args[4]) != "pc.singer" || str(c.Args[5]) != "pc.title" {
				probs = append(probs, fmt.Sprintf("延后排空处 runBackfillCover 应透传 pc.coverGen 与 (pc.name, pc.singer, pc.title)，实得 %s", str(c)))
			}
		}
	}

	// 检查 runBackfillCover 闭包签名、顶层语句次序与 p.startCover 调用
	params := flatParamNames(a.backfillCover)
	if len(params) != 6 {
		probs = append(probs, fmt.Sprintf("runBackfillCover 应恰有 6 个形参 (coverGen, sid, smid, name, singer, title)，实得 %v", params))
	} else {
		genParam, nameParam, singerParam, titleParam := params[0], params[3], params[4], params[5]
		fetchIdx := -1
		startIdx := -1
		var startCalls []*ast.CallExpr
		var fallbackIdxs []int
		for i, st := range a.backfillCover.Body.List {
			if as, ok := st.(*ast.AssignStmt); ok && len(as.Lhs) == 2 && len(as.Rhs) == 1 &&
				str(as.Lhs[0]) == "coverURL" && str(as.Lhs[1]) == "det" &&
				str(as.Rhs[0]) == "fetchCoverURL("+params[1]+", "+params[2]+")" {
				fetchIdx = i
			}
			if isCoverURLFallbackIf(st) {
				fallbackIdxs = append(fallbackIdxs, i)
			}
			if es, ok := st.(*ast.ExprStmt); ok {
				if c, ok := es.X.(*ast.CallExpr); ok && str(c.Fun) == "p.startCover" {
					startIdx = i
				}
			}
		}
		ast.Inspect(a.backfillCover.Body, func(x ast.Node) bool {
			switch x := x.(type) {
			case *ast.CallExpr:
				if str(x.Fun) == "p.startCover" {
					startCalls = append(startCalls, x)
				}
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					switch str(l) {
					case genParam, nameParam, singerParam, titleParam:
						probs = append(probs, "runBackfillCover 内不得改写形参 "+str(l))
					}
				}
			}
			return true
		})
		if fetchIdx < 0 {
			probs = append(probs, "runBackfillCover 顶层缺少 coverURL, det := fetchCoverURL(sid, smid)")
		}
		if len(startCalls) != 1 || startIdx < 0 {
			probs = append(probs, fmt.Sprintf("runBackfillCover 顶层应恰有 1 处 p.startCover 调用，实得 %d", len(startCalls)))
		} else {
			c := startCalls[0]
			if len(c.Args) != 6 || str(c.Args[0]) != "realQQCoverOps" || str(c.Args[1]) != genParam ||
				str(c.Args[2]) != "coverURL" || str(c.Args[3]) != nameParam || str(c.Args[4]) != singerParam || str(c.Args[5]) != titleParam {
				probs = append(probs, fmt.Sprintf("runBackfillCover 中 p.startCover 实参不符，期望 (%s, coverURL, %s, %s, %s)，实得 %s",
					genParam, nameParam, singerParam, titleParam, str(c)))
			}
		}
		hasFallbackInOrder := false
		for _, idx := range fallbackIdxs {
			if fetchIdx >= 0 && startIdx >= 0 && idx > fetchIdx && idx < startIdx {
				hasFallbackInOrder = true
			}
		}
		if !hasFallbackInOrder {
			probs = append(probs, `runBackfillCover 在 fetchCoverURL(sid, smid) 之后、p.startCover 之前缺少 if coverURL == "" { coverURL = currentCoverURL }`)
		}
	}

	return probs
}

// claimedNameProblems 把同一条规则扩到补取块之外（#218 变基后，适配 #229、#248）：补取块与播放中换词都经 showLyrics 发歌词，
// 它发出的 all_lyrics 与无歌词时的标题须取 currentTitle；currentTitle 只在换歌分支里赋值；currentName 与 currentSinger
// 只允许在换歌分支里赋值，或在补取块里经唯一允许的 currentName, currentSinger = backfillNames(...) 语句赋值；
// song_info_update 只在换歌分支与补取块里发（封面回写统一经独立方法 startCover 的 EmitForGen）。找不到锚点时自报失效。
func claimedNameProblems(src string) []string {
	a, fail := findQQSessionAnchors(src)
	if fail != "" {
		return []string{fail}
	}
	str := types.ExprString
	within := func(n ast.Node, b ast.Node) bool { return n.Pos() >= b.Pos() && n.End() <= b.End() }
	var probs []string
	ast.Inspect(a.run.Body, func(x ast.Node) bool {
		switch x := x.(type) {
		case *ast.AssignStmt:
			for _, l := range x.Lhs {
				switch str(l) {
				case "currentTitle":
					if !within(x, a.sw.Body) {
						probs = append(probs, "换歌分支之外给 currentTitle 赋值")
					}
				case "currentName", "currentSinger":
					if !within(x, a.sw.Body) && !(within(x, a.fill.Body) && isBackfillNamesAssign(x)) {
						probs = append(probs, "换歌分支与补取 backfillNames 之外给 "+str(l)+" 赋值")
					}
				}
			}
		case *ast.CallExpr:
			for _, arg := range x.Args {
				if str(arg) == "player.EventSongInfoUpdate" && !within(x, a.sw.Body) && !within(x, a.fill.Body) {
					probs = append(probs, "换歌分支与补取块之外发了 song_info_update")
				}
			}
		}
		return true
	})
	emits := 0
	ast.Inspect(a.show.Body, func(x ast.Node) bool {
		c, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch {
		case str(c.Fun) == "p.emitNoLyrics" && len(c.Args) > 0:
			emits++
			if str(c.Args[0]) != "currentTitle" {
				probs = append(probs, "showLyrics 无歌词一支的标题应取 currentTitle，实得 "+str(c.Args[0]))
			}
		case str(c.Fun) == "p.Emit" && len(c.Args) == 2 && str(c.Args[0]) == "player.EventAllLyrics":
			emits++
			title := ""
			if u, ok := c.Args[1].(*ast.UnaryExpr); ok {
				if cl, ok := u.X.(*ast.CompositeLit); ok {
					for _, el := range cl.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok && str(kv.Key) == "Title" {
							title = str(kv.Value)
						}
					}
				}
			}
			if title != "currentTitle" {
				probs = append(probs, "showLyrics 发 all_lyrics 的标题应取 currentTitle，实得 "+title)
			}
		}
		return true
	})
	if emits == 0 {
		probs = append(probs, "showLyrics 里没找到发歌词的调用 —— 门禁失效")
	}
	return probs
}

// TestBackfillKeepsClaimedNames 守住 runSession 认领分支、补取块与 runBackfillCover 的 #229 接线形状及补取块外的标题/名称不变式（Q-G）。
//
// 变异自证（隔离副本实测，直接改写 qqmusic.go 且均可编译）：
//   - 补取块给 currentTitle 赋值 → 本例红；
//   - 补取块歌名直取 lrcName → 本例红；
//   - 补取块删去 backfillNames 调用（不经开关取舍）→ 本例红；
//   - 补取块 NewSongGen 挪回 runBackfillCover 的 fetchCoverURL 之后 → 本例红；
//   - runBackfillCover 删去封面地址回退（保留 _ = currentCoverURL 使其可编译）→ 本例红；
//   - 认领分支删去 currentCoverURL = coverURL → 本例红；
//   - 认领分支 NewSongGen 挪回 p.Emit 之后（currentCoverURL 之前）→ 本例红（mut-229-final-r1 实测）。
func TestBackfillKeepsClaimedNames(t *testing.T) {
	src := readQQMusicSource(t)
	if probs := backfillNameProblems(src); len(probs) > 0 {
		t.Fatalf("补取块或认领分支的 #229 歌名/歌手/封面代次接线不符：\n  %s", strings.Join(probs, "\n  "))
	}
	if probs := claimedNameProblems(src); len(probs) > 0 {
		t.Fatalf("补取块之外发出的歌名、歌手或标题没有沿用认领时的：\n  %s", strings.Join(probs, "\n  "))
	}
}

func hasRealProblem(probs []string) bool {
	for _, p := range probs {
		if !strings.Contains(p, "门禁失效") && !strings.HasPrefix(p, "解析失败") {
			return true
		}
	}
	return false
}

// TestBackfillNameCatchesKnownHoles 以改写过的源码确认门禁拦得住 #229 各类缺陷变体。改写找不到原文时自报失效。
func TestBackfillNameCatchesKnownHoles(t *testing.T) {
	src := readQQMusicSource(t)
	const (
		backfillCall  = "\t\t\t\t\t\tcurrentName, currentSinger = backfillNames(currentName, currentSinger, meta.Name, meta.Singer, lrcName, lrcSinger)\n"
		namesBind     = "\t\t\t\t\t\tname, singer, title := currentName, currentSinger, currentTitle\n"
		fillGenLine   = "\t\t\t\t\t\tcoverGen := p.NewSongGen()\n"
		fillFetchLine = "\t\tcoverURL, det := fetchCoverURL(sid, smid)\n"
		fillEmitLine  = "\t\t\t\t\t\tp.Emit(player.EventSongInfoUpdate, &player.SongInfo{Name: name, Singer: singer, Title: title})\n"
		fillFallback  = "\t\tif coverURL == \"\" {\n\t\t\tcoverURL = currentCoverURL // 认领时的任务已作废：取不到新地址时用认领时的地址，这首仍有封面\n\t\t}\n"
		fillStartLine = "\t\tp.startCover(realQQCoverOps, coverGen, coverURL, name, singer, title)\n"
		fillSchedCall = "pendingCover.schedule(lyricSt.undecided(), assocKey.gen, coverGen, sid, smid, name, singer, title)"
		fillRunCall   = "\t\t\t\t\t\t\trunBackfillCover(coverGen, sid, smid, name, singer, title)\n"
		drainRunCall  = "\t\t\trunBackfillCover(pc.coverGen, pc.sid, pc.smid, pc.name, pc.singer, pc.title)\n"
		claimNames    = "\t\t\tcurrentName, currentSinger = meta.Name, meta.Singer\n"
		claimGenLine  = "\n\t\t\tcoverGen := p.NewSongGen()\n"
		claimCoverURL = "\t\t\tcurrentCoverURL = coverURL\n"
	)
	cases := []struct {
		name, needle string
		mutate       func(string) string
	}{
		{"歌名直接取 lrcName", namesBind, func(s string) string {
			return strings.Replace(s, namesBind, "\t\t\t\t\t\tname, singer, title := lrcName, currentSinger, currentTitle\n", 1)
		}},
		{"标题按歌词标签重拼", namesBind, func(s string) string {
			return strings.Replace(s, namesBind, "\t\t\t\t\t\tname, singer, title := currentName, currentSinger, lrcName+\" - \"+lrcSinger\n", 1)
		}},
		{"标题按本拍内存重拼", namesBind, func(s string) string {
			return strings.Replace(s, namesBind, "\t\t\t\t\t\tname, singer, title := meta.Name, meta.Singer, meta.Name+\" - \"+meta.Singer\n", 1)
		}},
		{"补取块给 currentTitle 赋值", namesBind, func(s string) string {
			return strings.Replace(s, namesBind, namesBind+"\t\t\t\t\t\tcurrentTitle = name\n", 1)
		}},
		{"不经 backfillNames 直接沿用认领值（开关失效）", backfillCall, func(s string) string {
			return strings.Replace(s, backfillCall, "", 1)
		}},
		{"NewSongGen 挪回 runBackfillCover 的 fetchCoverURL 之后", fillGenLine, func(s string) string {
			s = strings.Replace(s, fillGenLine, "", 1)
			return strings.Replace(s, fillFetchLine, fillFetchLine+"\t\tcoverGen := p.NewSongGen()\n", 1)
		}},
		{"NewSongGen 挪到 Emit 之后、封面分流之前", fillGenLine, func(s string) string {
			s = strings.Replace(s, fillGenLine, "", 1)
			return strings.Replace(s, fillEmitLine, fillEmitLine+fillGenLine, 1)
		}},
		{"删去封面地址回退", fillFallback, func(s string) string {
			return strings.Replace(s, fillFallback, "", 1)
		}},
		{"startCover 用了错误的代次", fillStartLine, func(s string) string {
			return strings.Replace(s, fillStartLine, "\t\tp.startCover(realQQCoverOps, 0, coverURL, name, singer, title)\n", 1)
		}},
		{"pendingCover.schedule 漏透传 coverGen", fillSchedCall, func(s string) string {
			return strings.Replace(s, fillSchedCall, "pendingCover.schedule(lyricSt.undecided(), assocKey.gen, 0, sid, smid, name, singer, title)", 1)
		}},
		{"补取 runBackfillCover 漏透传 coverGen", fillRunCall, func(s string) string {
			return strings.Replace(s, fillRunCall, "\t\t\t\t\t\t\trunBackfillCover(0, sid, smid, name, singer, title)\n", 1)
		}},
		{"延后排空 runBackfillCover 漏透传 pc.coverGen", drainRunCall, func(s string) string {
			return strings.Replace(s, drainRunCall, "\t\t\trunBackfillCover(0, pc.sid, pc.smid, pc.name, pc.singer, pc.title)\n", 1)
		}},
		{"认领分支删去 currentCoverURL = coverURL", claimCoverURL, func(s string) string {
			return strings.Replace(s, claimCoverURL, "", 1)
		}},
		{"认领分支删去 currentName, currentSinger = meta.Name, meta.Singer", claimNames, func(s string) string {
			return strings.Replace(s, claimNames, "", 1)
		}},
		{"认领分支 NewSongGen 挪回 Emit 之后（currentCoverURL 之前）", claimGenLine, func(s string) string {
			s = strings.Replace(s, claimGenLine, "\n", 1)
			return strings.Replace(s, claimCoverURL, "\t\t\tcoverGen := p.NewSongGen()\n"+claimCoverURL, 1)
		}},
		{"认领分支 NewSongGen 之后改写代次变量", claimCoverURL, func(s string) string {
			return strings.Replace(s, claimCoverURL, "\t\t\tcoverGen = 0\n"+claimCoverURL, 1)
		}},
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
			if !hasRealProblem(backfillNameProblems(mutated)) {
				t.Fatalf("门禁没有拦下这处改写（返回：%v）", backfillNameProblems(mutated))
			}
		})
	}
}

// TestClaimedNameCatchesKnownHoles 以改写过的源码确认 claimedNameProblems 拦得住补取块之外改写或另发歌名、歌手与标题，
// 且在补取块内仅放行 currentName, currentSinger = backfillNames(...)、仍严禁给 currentTitle 赋值或绕过 backfillNames 赋值。
// 改写找不到原文时自报失效。
func TestClaimedNameCatchesKnownHoles(t *testing.T) {
	src := readQQMusicSource(t)
	const (
		showTitle    = "Title: currentTitle, Duration: currentDurationSec, Position: pos"
		showNone     = "p.emitNoLyrics(currentTitle, currentDurationSec, pos, prog, lyricSt.hint())"
		showReset    = "\t\tcurrentLyrics = lines\n\t\tlastLineIdx = -1\n"
		tickShow     = "\t\t\tshowLyrics(lines, float32(anchorProgressMs)/1000.0, anchorTime)\n"
		backfillCall = "\t\t\t\t\t\tcurrentName, currentSinger = backfillNames(currentName, currentSinger, meta.Name, meta.Singer, lrcName, lrcSinger)\n"
	)
	cases := []struct {
		name, needle string
		mutate       func(string) string
	}{
		{"showLyrics 的 all_lyrics 标题另取", showTitle, func(s string) string {
			return strings.Replace(s, showTitle, "Title: lyricSt.title, Duration: currentDurationSec, Position: pos", 1)
		}},
		{"showLyrics 无歌词一支的标题另取", showNone, func(s string) string {
			return strings.Replace(s, showNone, "p.emitNoLyrics(lyricSt.title, currentDurationSec, pos, prog, lyricSt.hint())", 1)
		}},
		{"showLyrics 改写 currentTitle", showReset, func(s string) string {
			return strings.Replace(s, showReset, showReset+"\t\tcurrentTitle = lyricSt.title\n", 1)
		}},
		{"播放中换词改写 currentName", tickShow, func(s string) string {
			return strings.Replace(s, tickShow, tickShow+"\t\t\tcurrentName, currentSinger = lyricSt.name, lyricSt.singer\n", 1)
		}},
		{"播放中换词另发 song_info_update", tickShow, func(s string) string {
			return strings.Replace(s, tickShow, tickShow+"\t\t\tp.Emit(player.EventSongInfoUpdate, &player.SongInfo{Title: currentTitle})\n", 1)
		}},
		{"补取块改写 currentTitle", backfillCall, func(s string) string {
			return strings.Replace(s, backfillCall, backfillCall+"\t\t\t\t\t\tcurrentTitle = lrcName\n", 1)
		}},
		{"补取块绕过 backfillNames 直接给 currentName 赋值", backfillCall, func(s string) string {
			return strings.Replace(s, backfillCall, "\t\t\t\t\t\tcurrentName, currentSinger = lrcName, lrcSinger\n", 1)
		}},
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
			if !hasRealProblem(claimedNameProblems(mutated)) {
				t.Fatalf("门禁没有拦下这处改写（返回：%v）", claimedNameProblems(mutated))
			}
		})
	}
}
