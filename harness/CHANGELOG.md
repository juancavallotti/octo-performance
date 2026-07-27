# Changelog

## 0.1.0 (2026-07-27)


### Features

* **harness:** offer every scenario, and measure the rate instead of remembering it ([3d10f64](https://github.com/juancavallotti/octo-performance/commit/3d10f64d2e04b5ff0f26154b82385179a73eaef5))
* **harness:** put the subject on another machine, and measure the clock between them ([cca016d](https://github.com/juancavallotti/octo-performance/commit/cca016ddcbc48385337c429c6d72a2abaa084e2d))
* **harness:** roll a campaign up into one report that leads with a sentence ([8f139c2](https://github.com/juancavallotti/octo-performance/commit/8f139c20dc50b90d3271d1bdaed18c27ec431fae))
* **harness:** run a whole cell, gate it, and write it down ([15c01c5](https://github.com/juancavallotti/octo-performance/commit/15c01c582f201291fe229bd169aae0de5f2e2bc9))
* **harness:** run processes, ask artifacts what they accept, and offer load ([d6e10f5](https://github.com/juancavallotti/octo-performance/commit/d6e10f51c26e8480852344f59ea8e508816dc1da))


### Fixes

* **harness:** a corroborator may only veto what it can resolve ([52e96bc](https://github.com/juancavallotti/octo-performance/commit/52e96bc43bad8c967a53e3d7ff5ba78c52201676))
* **harness:** a corroborator that cannot answer must not veto the window ([f78066f](https://github.com/juancavallotti/octo-performance/commit/f78066f5541c588df98d24900c1f71d84b7a0550))
* **harness:** absolute scenario paths, and a peer check that compares like with like ([775e4ab](https://github.com/juancavallotti/octo-performance/commit/775e4ab0da6fe38c29dc24c88089f71f8f942a9a))
* **harness:** hand the subject absolute paths, and separate its staging root from ours ([43d2427](https://github.com/juancavallotti/octo-performance/commit/43d242796af14b1d02f3ea11dbc3e7eb1eda0fdb))
* **harness:** ship a lab, not a binary, and make the release config actually work ([2bc4fe7](https://github.com/juancavallotti/octo-performance/commit/2bc4fe7c932d6cd3b6bee4d9078a3a4298cfe5ab))
* **harness:** warm the capacity ramp, or it calibrates against its own cold start ([c4f6335](https://github.com/juancavallotti/octo-performance/commit/c4f6335f646ab3120bfc8bf46a81633e615c7a2d))


### Refactoring

* **harness:** drop the agent binary the SSH transport made unnecessary ([aa66a58](https://github.com/juancavallotti/octo-performance/commit/aa66a58df6d903e026ec76a813dae5845f952146))
* **harness:** move the Go module into harness/ and ship it as a release ([3fad134](https://github.com/juancavallotti/octo-performance/commit/3fad1343006a7f5efd464d2a19c1d76ee2446577))
