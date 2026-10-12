// Package dice 算「N 颗 F 面骰子」的点数分布和期望收益。
//
// 用途是给「投币摇骰子、点数决定奖金」这类玩法做**精确的收支核对**：
// 奖金表配好之后，先用 ExpectedValue 算出每局期望发出多少，
// 再和单价一比就知道返还率是多少、平台是赚还是赔，
// 不用靠抽样跑几百万次去估（抽样只能验「大致对」，验不出「差一点点」）。
//
// 概率不需要额外实现：点数之和的分布是骰子自带的。N 颗各 1..F 独立均匀，
// 一共 F^N 种结果，摇出总和 s 的走法数就是 Distribution 算出来的那一个。
//
//	d := dice.Distribution(3, 6)   // 3 颗六面骰：下标 3..18，值 1,3,6,10,15,21,25,27,...
//	dice.ExpectedValue(3, 6, func(sum int) float64 { return payout[sum] })
//
// 包内所有函数都是纯计算，不依赖随机数，也不依赖时间，可以放心并发调用。
package dice

// Total 是 N 颗 F 面骰子一共有多少种结果（F 的 N 次方）。
// 参数不合法（N、F 不是正数）时返回 0。
func Total(n, faces int) int {
	if n <= 0 || faces <= 0 {
		return 0
	}
	t := 1
	for i := 0; i < n; i++ {
		t *= faces
		if t <= 0 { // 溢出：当成「算不出来」，别返回一个负数
			return 0
		}
	}
	return t
}

// MinSum / MaxSum 是点数总和的最小值和最大值。
// 参数不合法时返回 0。
func MinSum(n, faces int) int {
	if n <= 0 || faces <= 0 {
		return 0
	}
	return n
}

func MaxSum(n, faces int) int {
	if n <= 0 || faces <= 0 {
		return 0
	}
	return n * faces
}

// Ways 是「N 颗 F 面骰子摇出总和 sum」有多少种走法。
// sum 落在 [n, n*faces] 之外时返回 0。
//
// 实现是逐颗递推（动态规划），**不是** N 层循环：
// 六颗六面骰是 46656 种结果，十颗就是 6000 万，嵌套循环会慢到没法用；
// 递推的复杂度是 O(N × F × 总和)，十颗也只跑几万次。
func Ways(n, faces, sum int) int {
	if n <= 0 || faces <= 0 {
		return 0
	}
	if sum < MinSum(n, faces) || sum > MaxSum(n, faces) {
		return 0
	}
	// cur[s] = 前 i 颗摇出总和 s 的走法数
	cur := make([]int, sum+1)
	for f := 1; f <= faces && f <= sum; f++ {
		cur[f] = 1
	}
	next := make([]int, sum+1)
	for i := 2; i <= n; i++ {
		for j := range next {
			next[j] = 0
		}
		for s := i; s <= sum; s++ {
			c := 0
			for f := 1; f <= faces; f++ {
				if s-f < i-1 {
					break
				}
				c += cur[s-f]
			}
			next[s] = c
		}
		cur, next = next, cur
	}
	return cur[sum]
}

// Distribution 返回从 MinSum 到 MaxSum 每一档总和的走法数：
//
//	d := Distribution(3, 6)
//	d[3] 是总和 3 的走法数，d[18] 是总和 18 的走法数
//
// 返回值长度是 MaxSum+1（下标就是总和，前面几格用不到的是 0），
// 这样调用方可以直接拿总和当下标，不用再减偏移。
// 参数不合法时返回 nil。
func Distribution(n, faces int) []int {
	if n <= 0 || faces <= 0 {
		return nil
	}
	max := MaxSum(n, faces)
	out := make([]int, max+1)
	// 一颗骰子的分布，然后一颗一颗卷上去。
	for f := 1; f <= faces; f++ {
		out[f] = 1
	}
	next := make([]int, max+1)
	for i := 2; i <= n; i++ {
		for j := range next {
			next[j] = 0
		}
		for s := i; s <= max; s++ {
			c := 0
			for f := 1; f <= faces; f++ {
				if s-f < i-1 {
					break
				}
				c += out[s-f]
			}
			next[s] = c
		}
		out, next = next, out
	}
	return out
}

// ExpectedValue 算每局期望发出的奖金：Σ（该总和的走法数 × 该总和的奖金）÷ 总结果数。
//
// payout 收点数总和、返回这一档给多少（币 / 分 / 积分都行，单位跟着 payout 走）。
// payout 为 nil、或者参数不合法时返回 0。
//
// 一局的返还率 = ExpectedValue(...) ÷ 一局单价，调用方自己除。
func ExpectedValue(n, faces int, payout func(sum int) float64) float64 {
	if payout == nil {
		return 0
	}
	total := Total(n, faces)
	if total <= 0 {
		return 0
	}
	dist := Distribution(n, faces)
	if dist == nil {
		return 0
	}
	sum := 0.0
	for s := MinSum(n, faces); s <= MaxSum(n, faces); s++ {
		if dist[s] == 0 {
			continue
		}
		sum += float64(dist[s]) * payout(s)
	}
	return sum / float64(total)
}

// Probability 是摇出某个总和的概率（0 ~ 1）。
// 总和越界、参数不合法都返回 0。
func Probability(n, faces, sum int) float64 {
	total := Total(n, faces)
	if total <= 0 {
		return 0
	}
	return float64(Ways(n, faces, sum)) / float64(total)
}
