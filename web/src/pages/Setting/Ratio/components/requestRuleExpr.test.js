import { describe, expect, test } from 'bun:test';

import {
  buildRequestRuleExpr,
  MATCH_GTE,
  MATCH_RANGE,
  SOURCE_TIME,
  tryParseRequestRuleExpr,
} from './requestRuleExpr.js';

function timeRange(start, end, timeFunc = 'hour') {
  return [{
    conditions: [{
      source: SOURCE_TIME,
      timeFunc,
      timezone: 'Asia/Shanghai',
      mode: MATCH_RANGE,
      value: '',
      rangeStart: start,
      rangeEnd: end,
    }],
    multiplier: '2',
  }];
}

describe('time billing rule expressions', () => {
  test('uses AND for a within-day range', () => {
    expect(buildRequestRuleExpr(timeRange('9', '12'))).toBe(
      '(hour("Asia/Shanghai") >= 9 && hour("Asia/Shanghai") < 12 ? 2 : 1)',
    );
  });

  test('uses OR for an overnight range', () => {
    expect(buildRequestRuleExpr(timeRange('21', '6'))).toBe(
      '(hour("Asia/Shanghai") >= 21 || hour("Asia/Shanghai") < 6 ? 2 : 1)',
    );
  });

  test('rejects fractional and out-of-range values', () => {
    expect(buildRequestRuleExpr(timeRange('9.5', '12'))).toBe('');
    expect(buildRequestRuleExpr(timeRange('9', '24'))).toBe('');
    expect(buildRequestRuleExpr([{
      conditions: [{
        source: SOURCE_TIME,
        timeFunc: 'weekday',
        timezone: 'UTC',
        mode: MATCH_GTE,
        value: '7',
      }],
      multiplier: '2',
    }])).toBe('');
  });

  test('round-trips within-day and legacy overnight ranges', () => {
    for (const expr of [
      '(hour("Asia/Shanghai") >= 9 && hour("Asia/Shanghai") < 12 ? 2 : 1)',
      '(hour("Asia/Shanghai") >= 21 || hour("Asia/Shanghai") < 6 ? 2 : 1)',
    ]) {
      const parsed = tryParseRequestRuleExpr(expr);
      expect(parsed).not.toBeNull();
      expect(parsed[0].conditions).toHaveLength(1);
      expect(parsed[0].conditions[0].mode).toBe(MATCH_RANGE);
      expect(buildRequestRuleExpr(parsed)).toBe(expr);
    }
  });
});
