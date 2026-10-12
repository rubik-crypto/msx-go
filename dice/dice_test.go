package dice

import (
	"math"
	"testing"
)

// 三颗六面骰的走法数是掷骰子最经典的一串数（1,3,6,10,15,21,25,27,27,25,...），
// 拿它当死数据对答案：算错了这一串立刻对不上。
func TestDistribution3d6(t *testing.T) {
	want := map[int]int{
		3: 1, 4: 3, 5: 6, 6: 10, 7: 15, 8: 21, 9: 25,
		10: 27, 11: 27, 12: 25, 13: 21, 14: 15, 15: 10,
		16: 6, 17: 3, 18: 1,
	}
	d := Distribution(3, 6)
	if len(d) != 19 {
		t.Fatalf("分布长度 = %d，应该是 19（下标 0..18）", len(d))
	}
	sum := 0
	for s, w := range want {
		if d[s] != w {
			t.Fatalf("总和 %d 的走法 = %d，应该是 %d", s, d[s], w)
		}
		sum += w
	}
	if sum != 216 {
		t.Fatalf("走法总数 = %d，应该是 216", sum)
	}
	if Total(3, 6) != 216 {
		t.Fatalf("Total(3,6) = %d", Total(3, 6))
	}
}

func TestWaysMatchesDistribution(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5} {
		for _, f := range []int{2, 4, 6, 10} {
			d := Distribution(n, f)
			for s := MinSum(n, f) - 1; s <= MaxSum(n, f)+1; s++ {
				got := Ways(n, f, s)
				var want int
				if s >= 0 && s < len(d) {
					want = d[s]
				}
				if s < MinSum(n, f) || s > MaxSum(n, f) {
					want = 0
				}
				if got != want {
					t.Fatalf("Ways(%d,%d,%d) = %d，应该是 %d", n, f, s, got, want)
				}
			}
		}
	}
}

// 一列的走法数加起来必须等于 F 的 N 次方（所有结果都被数到了）。
func TestWaysSumEqualsTotal(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 6} {
		for _, f := range []int{2, 6, 8} {
			sum := 0
			for s := MinSum(n, f); s <= MaxSum(n, f); s++ {
				sum += Ways(n, f, s)
			}
			if sum != Total(n, f) {
				t.Fatalf("%d 颗 %d 面：走法合计 %d，Total = %d", n, f, sum, Total(n, f))
			}
		}
	}
}

// 分布必须对称：总和 s 和 (n+1)*... 也就是最左最右配对，走法数一样。
func TestDistributionSymmetric(t *testing.T) {
	for _, n := range []int{1, 3, 5} {
		for _, f := range []int{4, 6} {
			lo, hi := MinSum(n, f), MaxSum(n, f)
			for s := lo; s <= hi; s++ {
				if Ways(n, f, s) != Ways(n, f, lo+hi-s) {
					t.Fatalf("%d 颗 %d 面：总和 %d 和 %d 的走法不对称", n, f, s, lo+hi-s)
				}
			}
		}
	}
}

func TestExpectedValue(t *testing.T) {
	// 每档都发同一个数：期望就等于那个数（和分布无关）。
	flat := ExpectedValue(3, 6, func(int) float64 { return 7 })
	if math.Abs(flat-7) > 1e-9 {
		t.Fatalf("固定奖金 7 的期望 = %v", flat)
	}
	// 一颗六面骰，只对 6 点发 60：期望 = 60/6 = 10。
	one := ExpectedValue(1, 6, func(s int) float64 {
		if s == 6 {
			return 60
		}
		return 0
	})
	if math.Abs(one-10) > 1e-9 {
		t.Fatalf("一颗骰子中 6 发 60 的期望 = %v，应该是 10", one)
	}
	// 空表必须是 0，不能出现除零。
	if v := ExpectedValue(0, 0, func(int) float64 { return 1 }); v != 0 {
		t.Fatalf("参数不合法时应该返回 0，得到 %v", v)
	}
	if v := ExpectedValue(3, 6, nil); v != 0 {
		t.Fatalf("payout 为 nil 时应该返回 0，得到 %v", v)
	}
}

func TestProbability(t *testing.T) {
	// 三颗六面骰摇出 10 点的概率是 27/216。
	if p := Probability(3, 6, 10); math.Abs(p-27.0/216) > 1e-12 {
		t.Fatalf("P(3d6 = 10) = %v", p)
	}
	if p := Probability(3, 6, 2); p != 0 {
		t.Fatalf("摇不出 2 点，概率应该是 0，得到 %v", p)
	}
	// 全部结果的概率加起来是 1。
	sum := 0.0
	for s := MinSum(3, 6); s <= MaxSum(3, 6); s++ {
		sum += Probability(3, 6, s)
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("概率合计 = %v", sum)
	}
}

// 大一点的点数也要算得动、算得对（递推实现的意义就在这儿）。
func TestManyDice(t *testing.T) {
	const n, f = 10, 6
	sum := 0
	dist := Distribution(n, f)
	for s := MinSum(n, f); s <= MaxSum(n, f); s++ {
		sum += dist[s]
	}
	if sum != Total(n, f) {
		t.Fatalf("十颗六面骰的走法合计 = %d，Total = %d", sum, Total(n, f))
	}
	if dist[MinSum(n, f)] != 1 || dist[MaxSum(n, f)] != 1 {
		t.Fatalf("两头应该各只有 1 种走法")
	}
}
