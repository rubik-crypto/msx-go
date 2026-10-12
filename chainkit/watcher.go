package chainkit

import (
	"context"
	"fmt"
)

// watch 扫描主循环。链无关：只依赖 source 接口，所以加一条链不需要动这里。
//
// 每轮的顺序是固定的：
//
//	探测首选端点回归 → 取链头 → 算目标高度(head - Confirmations，默认就是 head)
//	→ 处理「链头低于游标」→ 处理「落后过多」→ 扫一段 → 推进并保存游标
//
// 游标的语义是「已经扫到哪个高度」。它只在整段扫描成功之后才前进，
// 所以任何中途失败都会让下一轮重扫该区间——回调因此是 at-least-once。
//
// 这里**不等确认数**：默认一路追到链头，看到转账就回调。等待确认、决定这笔充值
// 最终是否成立，是调用方的事（它才有记录未确认项的那张表）。
func (c *Chain) watch(ctx context.Context) error {
	cfg := c.cfg
	logf := cfg.logf

	cursor, known, err := cfg.Cursor.Load(ctx)
	if err != nil {
		return fmt.Errorf("chainkit[%s]: 读取游标失败: %w", cfg.Chain, err)
	}
	if !known {
		start, err := c.initialStart(ctx)
		if err != nil {
			return fmt.Errorf("chainkit[%s]: 确定起始高度失败: %w", cfg.Chain, err)
		}
		cursor = start - 1 // 游标是「已处理到」，所以起点要减一
		if err := cfg.Cursor.Save(ctx, cursor); err != nil {
			return fmt.Errorf("chainkit[%s]: 保存初始游标失败: %w", cfg.Chain, err)
		}
		logf("chainkit[%s]: 首次启动，从区块 %d 开始扫描", cfg.Chain, start)
	} else {
		logf("chainkit[%s]: 从游标 %d 续扫", cfg.Chain, cursor)
	}

	// 取链头和扫描各自计数：合成一个计数器的话，链头查询成功会把扫描失败的次数抹掉，
	// 持续失败的扫描就永远退避在 1 秒，等于不停地敲一个已经在拒绝你的节点。
	headFails, scanFails := 0, 0
	for {
		if ctx.Err() != nil {
			logf("chainkit[%s]: 停止扫描（游标 %d）", cfg.Chain, cursor)
			return ctx.Err()
		}

		// 当前不在首选端点时，先用一次廉价调用试试它回来没有
		c.src.Probe(ctx)

		head, err := c.src.Head(ctx)
		if err != nil {
			headFails++
			logf("chainkit[%s]: 获取链头失败（连续 %d 次）: %v", cfg.Chain, headFails, err)
			if !sleepCtx(ctx, c.backoff(headFails)) {
				return ctx.Err()
			}
			continue
		}
		headFails = 0

		// 目标高度。Confirmations 默认 0，即一路扫到链头——库看到转账就回调，
		// 不等确认数（确认是充值业务自己的事）。设 >0 只是把最近这些块挡在回调之外，
		// 用来滤掉浅层重组带来的噪音，不构成任何「已确认」保证。
		var target uint64
		if head >= cfg.Confirmations {
			target = head - cfg.Confirmations
		}

		if head < cursor {
			c.handleBehind(ctx, head, target, &cursor)
			if !sleepCtx(ctx, cfg.interval) {
				return ctx.Err()
			}
			continue
		}

		// 落后过多：快进而不是逐块追赶，否则长期停机后永远追不上
		if cfg.maxCatchup > 0 && target > cursor+cfg.maxCatchup {
			skipped := target - cursor - cfg.maxCatchup
			cursor = target - cfg.maxCatchup
			logf("chainkit[%s]: 落后 %d 个区块，超过追赶上限，快进到 %d（跳过区间内的充值不会回调）",
				cfg.Chain, skipped, cursor)
			if err := cfg.Cursor.Save(ctx, cursor); err != nil {
				return fmt.Errorf("chainkit[%s]: 保存游标失败: %w", cfg.Chain, err)
			}
		}

		if target <= cursor {
			if !sleepCtx(ctx, cfg.interval) {
				return ctx.Err()
			}
			continue
		}

		to := min(target, cursor+cfg.batch)
		// OnTransfer 在这里被闭包绑定上 ctx：出错时要能因 ctx 取消而立刻收手
		emit := func(t *Transfer) error { return cfg.OnTransfer(ctx, t) }
		if err := c.src.ScanRange(ctx, cursor+1, to, emit); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			scanFails++
			logf("chainkit[%s]: 扫描 %d-%d 失败（连续 %d 次）: %v",
				cfg.Chain, cursor+1, to, scanFails, err)
			if !sleepCtx(ctx, c.backoff(scanFails)) {
				return ctx.Err()
			}
			continue
		}
		scanFails = 0

		cursor = to
		if err := cfg.Cursor.Save(ctx, cursor); err != nil {
			return fmt.Errorf("chainkit[%s]: 保存游标失败: %w", cfg.Chain, err)
		}
	}
}

// handleBehind 链头低于游标时的处理。
//
// 两种成因必须区分开，因为正确的反应完全相反：
//   - 正在用的就是首选端点 → 链自己回滚了那么深，游标停在旧高度会让替换块里的
//     新充值永远扫不到，所以要回退重扫（重复回调由调用方按 Key 去重吸收）。
//   - 用的是备用端点 → 只是这个节点落后，回退游标会把已回调的区间整段重放，
//     什么都没修好。等它追上就行。
func (c *Chain) handleBehind(ctx context.Context, head, target uint64, cursor *uint64) {
	cfg := c.cfg
	if !c.src.Preferred() {
		cfg.logf("chainkit[%s]: 链头 %d 低于游标 %d（备用端点尚未追上？），等待", cfg.Chain, head, *cursor)
		return
	}

	rewound := uint64(0)
	if target > cfg.lookback {
		rewound = target - cfg.lookback
	}
	cfg.logf("chainkit[%s]: 首选节点链头 %d 低于游标 %d，判定为深层回滚，游标回退到 %d 重扫",
		cfg.Chain, head, *cursor, rewound)
	*cursor = rewound
	// 保存失败不影响本轮：回退值下轮会再算一次，是幂等的
	if err := cfg.Cursor.Save(ctx, rewound); err != nil {
		cfg.logf("chainkit[%s]: 保存回退后的游标失败: %v", cfg.Chain, err)
	}
}

// initialStart 首次启动（没有游标）时的起始高度。
// StartBlock 优先；否则回溯 Lookback 个块——回溯而不是直接跳到链头，
// 是为了覆盖「停机期间到账」的充值。
func (c *Chain) initialStart(ctx context.Context) (uint64, error) {
	if c.cfg.StartBlock > 0 {
		return c.cfg.StartBlock, nil
	}
	head, err := c.src.Head(ctx)
	if err != nil {
		return 0, err
	}
	if head <= c.cfg.lookback {
		return 1, nil
	}
	return head - c.cfg.lookback + 1, nil
}
