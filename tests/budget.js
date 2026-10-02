'use strict';
// Time budgets for the report's speed tests. CI runners are slower and
// noisier than a laptop, so their budgets are scaled; SP_PERF_SCALE overrides.
const scale = Number(process.env.SP_PERF_SCALE) || (process.env.CI ? 4 : 1);

module.exports = { budget: ms => ms * scale };
