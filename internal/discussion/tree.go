package discussion

import "sort"

// BuildTree assembles comments into a forest; siblings keep input order.
func BuildTree(comments []Comment, likes map[Target]int) []*CommentNode {
	nodes := make(map[string]*CommentNode, len(comments))
	for _, c := range comments {
		nodes[c.ID] = &CommentNode{
			Comment: c,
			Likes:   likes[Target{Type: TargetComment, ID: c.ID}],
			Replies: []*CommentNode{},
		}
	}

	roots := make([]*CommentNode, 0, len(comments))
	for _, c := range comments {
		parent, ok := nodes[deref(c.ParentID)]
		if ok {
			parent.Replies = append(parent.Replies, nodes[c.ID])
			parent.ReplyCount++
		} else {
			roots = append(roots, nodes[c.ID])
		}
	}
	return roots
}

// SelectPage pages top-level comments by keyset on id and trims replies below
// MaxDepth.
func SelectPage(roots []*CommentNode, p Page) ([]*CommentNode, PageInfo) {
	info := PageInfo{Limit: p.Limit, TotalTopLevel: len(roots)}

	start := 0
	if p.Cursor != "" {
		start = sort.Search(len(roots), func(i int) bool { return roots[i].ID > p.Cursor })
	}
	end := min(start+p.Limit, len(roots))
	window := roots[start:end]

	if end < len(roots) && len(window) > 0 {
		info.HasMore = true
		info.NextCursor = window[len(window)-1].ID
	}

	if p.MaxDepth > 0 {
		// Walk visits a node before queueing its replies, so clearing them here
		// prunes the walk.
		Walk(window, func(n *CommentNode, level int) {
			if level >= p.MaxDepth {
				n.Replies = []*CommentNode{}
			}
		})
	}
	return window, info
}

// Walk visits nodes breadth-first with their level (top-level is 1). It is
// iterative so caller-controlled depth cannot exhaust the stack.
func Walk(roots []*CommentNode, visit func(node *CommentNode, level int)) {
	type entry struct {
		node  *CommentNode
		level int
	}

	queue := make([]entry, 0, len(roots))
	for _, r := range roots {
		queue = append(queue, entry{r, 1})
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		visit(cur.node, cur.level)
		for _, reply := range cur.node.Replies {
			queue = append(queue, entry{reply, cur.level + 1})
		}
	}
}

func ComputeEngagement(post Post, tree []*CommentNode, likes []Like) Engagement {
	e := Engagement{PostID: post.ID, CommentsByDepth: map[int]int{}}

	engaged := map[string]struct{}{}
	for _, l := range likes {
		if l.Target.Type == TargetPost {
			e.PostLikes++
		} else {
			e.CommentLikes++
		}
		engaged[l.UserID] = struct{}{}
	}
	Walk(tree, func(node *CommentNode, level int) {
		e.CommentCount++
		e.CommentsByDepth[level]++
		e.LongestReplyChain = max(e.LongestReplyChain, level)
		engaged[node.AuthorID] = struct{}{}
	})
	e.DistinctEngagedUsers = len(engaged)

	return e
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
