# Changelog

## [0.1.2](https://github.com/juancavallotti/octo-performance/compare/harness/v0.1.1...harness/v0.1.2) (2026-07-28)


### Fixes

* **harness:** a generator that dies must not leave the campaign waiting ([af6def5](https://github.com/juancavallotti/octo-performance/commit/af6def52fb9e0caa461f9bee3885e628c3b20a3d))
* **harness:** make the campaign directory absolute before calibration takes it ([3ceecd9](https://github.com/juancavallotti/octo-performance/commit/3ceecd9676abb747a07522e1178c11adff80799c))
* **infra:** forward packets to the container, or Postgres answers only itself ([721671b](https://github.com/juancavallotti/octo-performance/commit/721671b6667f16d2aabf9b7b759da6a2023cf4f0))

## [0.1.1](https://github.com/juancavallotti/octo-performance/compare/harness/v0.1.0...harness/v0.1.1) (2026-07-28)


### Fixes

* **harness:** exec env, not env exec, or the remote command dies at 127 ([a521132](https://github.com/juancavallotti/octo-performance/commit/a52113250014cc65a67989e532404179c8f21bd8))
* **harness:** exec env, not env exec, or the remote command dies at 127 ([f25b00a](https://github.com/juancavallotti/octo-performance/commit/f25b00a2ce3b86d51fec0b79dc3a4883ac3f1b8e))


### Refactoring

* **harness:** write the parts, do not concatenate then write ([cb4807f](https://github.com/juancavallotti/octo-performance/commit/cb4807f8539a3f0be977eed53afeffa973844565))

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
