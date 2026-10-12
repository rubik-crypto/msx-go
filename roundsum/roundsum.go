// Package roundsum 解决「页面上写出来的分项相加对不上合计」这件事。
//
// 页面上凡是把一笔总数拆成几项给人看的（资产分布、算力构成、投票占比、
// 账单明细、进度条档位），都会碰到同一类投诉：「1.01 + 2.02 怎么等于 3.04」。
// 根因不是算错了，而是**先算合计、再把各项各自圆一次**——两次圆整凑不到一起。
//
// 正确的做法是反过来：**先把每个分项圆成页面上要显示的那一位，再拿圆过的值
// 相加得出合计**，这样用户在屏幕上按计算器就是对的。本包提供这套算法。
//
// 三个入口：
//
//	Round(v, digits)              把单个值圆到页面上要显示的小数位
//	Parts(parts, digits)          分项各自圆整 + 合计（合计 = 圆过的分项之和）
//	Split(total, weights, digits) 把一笔总数按权重切开，各份之和正好等于合计
//
// 用法（服务端算好下发，页面照原样渲染，不要再圆一次）：
//
//	rounded, total := roundsum.Parts([]float64{base, node, hold}, 2)
//	// 页面上写 rounded[0], rounded[1], rounded[2], total —— 相加一定相等
//
// 注意：调用方拿到结果后**页面必须按同样的精度渲染**（两位小数就两位），
// 页面再自己 round 一次或换成别的精度，又会对不上。
package roundsum

import "math"

// Round 把 v 圆到 digits 位小数（四舍五入）。
//
// digits 为 0 时返回整数；digits 为负或超过 9 时按 0 和 9 处理
// （浮点数在这个范围之外已经没有可靠的十进制精度了）。
//
// 里面加了一个极小的相对容差：十进制里写 1.005 的数是存不成精确值的
// （实际是 1.00499999…），直接乘再四舍五入会得到 1.00，和人的预期不符；
// 容差只在这条半格边界附近起作用，正常数值分毫不动。
func Round(v float64, digits int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	s := scaleOf(digits)
	return math.Round(v*s+sign(v)*math.Abs(v)*1e-12) / s
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

// Parts 把每个分项先圆到 digits 位，再返回它们的合计。
//
// 返回值 total 等于 rounded 里各数之和（而不是原始值之和再圆一次），
// 所以「页面上几个分项相加 == 页面上写的合计」是精确成立的。
// 传 nil 或空切片时返回空切片和 0。
func Parts(parts []float64, digits int) (rounded []float64, total float64) {
	if len(parts) == 0 {
		return []float64{}, 0
	}
	s := scaleOf(digits)
	rounded = make([]float64, len(parts))
	// 加起来时按「整数单位」加，避免浮点尾巴（0.1+0.2=0.30000000000000004）——
	// 单位数是整数，整数加法是精确的。
	units := int64(0)
	for i, p := range parts {
		// 每一份都走 Round（口径和页面上要显示的那一位完全一致），
		// 再加起来——所以页面上「分项相加 = 合计」是精确成立的。
		u := int64(math.Round(Round(p, digits) * s))
		units += u
		rounded[i] = float64(u) / s
	}
	return rounded, float64(units) / s
}

// Split 把 total 按 weights 切成 len(weights) 份，每份圆到 digits 位，
// 并且**各份之和正好等于 Round(total, digits)**。
//
// 用的是「最大余数法」：先按权重取整，剩余的单位按小数部分从大到小补，
// 小数部分相同就按权重大的优先，再相同按序号在前的优先（结果稳定、可复现）。
//
// 权重之和为 0（或者全是负数）时按平均分；len(weights) == 0 时返回 nil。
// total 为负数时按绝对值分配、再整体取负，保证和等于圆整后的 total。
func Split(total float64, weights []float64, digits int) []float64 {
	n := len(weights)
	if n == 0 {
		return nil
	}
	s := scaleOf(digits)
	neg := total < 0
	if neg {
		total = -total
	}
	totalUnits := int64(math.Round(Round(total, digits) * s))

	sumW := 0.0
	for _, w := range weights {
		if w > 0 {
			sumW += w
		}
	}
	// 权重不合法（全 0 / 有负数）时退化成平均分，别把总数吞掉。
	useEqual := sumW <= 0
	if useEqual {
		sumW = float64(n)
	}

	units := make([]int64, n)
	frac := make([]float64, n)
	var used int64
	for i, w := range weights {
		if useEqual {
			w = 1
		}
		if w < 0 {
			w = 0
		}
		exact := float64(totalUnits) * (w / sumW)
		base := math.Floor(exact)
		units[i] = int64(base)
		frac[i] = exact - base
		used += units[i]
	}
	// 剩下的单位按小数部分从大到小补（最大余数法）。
	for left := totalUnits - used; left > 0; left-- {
		best := -1
		for i := range weights {
			if best < 0 {
				best = i
				continue
			}
			if frac[i] > frac[best] {
				best = i
				continue
			}
			// 小数部分相同时比权重，再相同比序号（结果稳定）。
			if frac[i] == frac[best] {
				wi, wb := weights[i], weights[best]
				if useEqual {
					wi, wb = 1, 1
				}
				if wi > wb {
					best = i
				}
			}
		}
		if best < 0 {
			break
		}
		units[best]++
		frac[best] = -1 // 补过的排到后面去，避免一直补同一项
	}
	out := make([]float64, n)
	for i, u := range units {
		if neg {
			u = -u
		}
		out[i] = float64(u) / s
	}
	return out
}

// Percent 把一组权重换成整数百分比，各份相加正好等于 100。
//
// 页面上写「占 33% / 占 33% / 占 33%」合计只有 99% 时会被人截图来问，
// 这个函数就是为了让那个 100 永远成立（多出来的那个点给权重最大的那一项）。
// 权重之和为 0 时按平均分。
func Percent(weights []float64) []int {
	parts := Split(100, weights, 0)
	out := make([]int, len(parts))
	for i, v := range parts {
		out[i] = int(v)
	}
	return out
}

func scaleOf(digits int) float64 {
	if digits < 0 {
		digits = 0
	}
	if digits > 9 {
		digits = 9
	}
	s := 1.0
	for i := 0; i < digits; i++ {
		s *= 10
	}
	return s
}
