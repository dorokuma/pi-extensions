# pigsdk (Go SDK 本地镜像)

这是本地开发/测试用的 SDK 镜像，PiG 构建扩展时并不用它，而是注入自己 staged 的 SDK。

## 元数据信息

- **来源绝对路径**：`/root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef`
- **pig 版本**：`0.5.2+1.4.0`（`/root/.local/bin/pig --version` 输出原文）
- **复制时间**：`2026-10-23 15:37:25 CST`
- **原始 SDK 文件数**：171 个文件（不含本地自维护的 `README.md`；本地目录总计 172 个文件）
- **原始 SDK 目录数**：15 个目录（含根目录）
- **原始 SDK 总字节数（实测）**：
  - 视在字节数（Apparent bytes / 文件内容合计）：`1,611,117` 字节（由 `du -sb` 与 `find . -type f ! -name README.md -printf '%s\n' | awk '{s+=$1} END {print s}'` 实测；早期文档曾误记为双倍值（1,611,117 × 2），已按实测更正）
  - 磁盘占用（Disk usage）：`2,060,288` 字节（由 `du -s --block-size=1` 实测，按 4096 块对齐占用 503 个块）

## 镜像说明

PiG 在编译扩展时会使用 `GOWORK=off`，并通过临时 modfile（`-modfile=.pig-build-*.mod`）将 `github.com/MichaelKinsy/PiG/extensions/sdk` 动态 replace 指向其自身 staged 的 SDK 目录（如 `/root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef`）。
本目录仅作为本地离线开发、静态检查（`go vet`）、单元测试（`go test`）以及 IDE 补全的 workspace 镜像源（配合根目录 `go.work`），PiG 构建扩展时并不使用本目录。

## SDK 校验与防漂移机制 (make sdk-check)

为防止本地镜像与 PiG 运行时使用的 staged SDK 发生版本或接口漂移，工程在根目录 Makefile 中提供了 `make sdk-check` 门禁目标：

- **用途**：自动选取宿主环境 `/root/.pig-test/state/pigsdk/` 下 glob 命中（排除 sdk-py-* / sdk-rs-*）的最后一个 `sdk-*` 目录（当前环境只有一个 Go staged SDK：`sdk-0c5774f119b9fdef`；多版本共存时应自行确认选中对象），并将本镜像目录与 staged SDK 目录下的全部文件执行逐文件 md5 校验。
- **判据**：严格一致性。校验过程中仅允许本仓库自维护的顶层说明文件 `README.md` 存在缺失或差异；其余任何文件的新增、缺失或 md5 不匹配均会触发非零退出并中断门禁。若在未部署 PiG staged SDK 的外部环境下运行，该目标会自动打印 `SKIP: staged SDK not found` 并以状态码 0 退出，避免环境不兼容导致阻断。
- **同步守则**：pig 升级后若 sdk-check 报差异，需重新同步 go/pigsdk 并更新本文件 md5 清单。

## SDK 原始文件 md5sum 清单

以下为复制完成后原始 SDK 文件（不含本 README.md）执行 `cd go/pigsdk && find . -type f | sort | xargs md5sum` 的输出原文：

```text
39fbf1e14cf9d29a5f3731f1af70206d  ./autocomplete.go
fd29d809c877bdefd96bc013e6d0565f  ./branch_summary.go
101c0076348a459906a6238cce11d376  ./builtin_tool.go
9da5828c1c556c9f85bc690996ae0c12  ./child_session.go
de38fd4bf27716b48452b6a0b62f2ab6  ./command_options.go
c1272526e0d16c507aab7bd1e5d407d8  ./compaction_cutpoint.go
a54b173d6a3a67b9ad0be14706febd06  ./compaction_estimate.go
63b4aa7fbf98c6db8afe321553496789  ./compaction_usage.go
d9484a4b3904f33d22e90dcf8cf12fcd  ./context_event.go
10fe65518dae99bc4efbfa4cfdcd355f  ./context.go
fa49cc551187fbd55d948e6e45115d5a  ./credential.go
5a46e5e21bae70b9968287496b4a7117  ./doc.go
22773285abbd6df3b54ee51009bc3a41  ./editor_component.go
52a7e9fd2eb26907c9f627f3311011f3  ./editor_hooks.go
772a1908183ac1a84e9fa12d5e955c86  ./event_args_memo.go
12313047fdff34c8385ce003eaceb11a  ./event_bus.go
fccf39e7cb8e2d7b6add2efc51f6c8e2  ./event_message.go
a85fcb1c84faf2e4c6bc223c3caedbc1  ./events.go
a789144ce0d7d001c9ea863ac2e81804  ./event_stream.go
22ea997d2d2ef52e489fc70f91ca9ec4  ./extension.go
43d55d8516205c6ad88a7adf71128364  ./file_mutation_queue.go
7ea1c899feea147b9d07cd53d950e1cd  ./footer_data.go
6f4415064eaaf205e305cc49a6654fd9  ./frontend/frontend.go
b45ba4b1f1c9296e926fadd0f20b3b45  ./frontend/view.go
7cd86c46d13441feb842d21a59017b25  ./go.mod
fc40aaf336a356fc6e8c21c3cacc94f1  ./host_helpers.go
2f2aff3855cedf4f1101ba4749a45148  ./host_info.go
68e8fc95ee76eaaa2508ed06960c34f6  ./host_state_subscriptions.go
4965088e3bda4249149001bec19e0859  ./image_dimension_note.go
e184ae772e16d644ddcb506e2797d22a  ./internal/jsloop/doc.go
436a31c66a88ceac0fe5d9a40d1c079c  ./internal/jsloop/lane.go
876205159207cdce31d31d9f07c11012  ./internal/jsloop/settle_gate.go
ddba7e119266afe1cce2b9bacdc7d7ee  ./internal/kitwire/encode.go
f989d9a692b2eddd602eb7e42b4d52d9  ./internal/kitwire/wire.go
49cf19ce2e75a9d6dfc0a5b56e149a51  ./json/decode.go
74bf9998a4e119cc3666788ab49e505d  ./json/encode.go
df71e24897f0a104727314a4c00ad829  ./json/fold.go
bcbbe46396f695d6c146fa8425a8650f  ./json/indent.go
7998cb338f82d15c0eff93b7004d272a  ./json/LICENSE
fdad17ae7f8e41bd21b0ea0f1dd2f710  ./json/scanner.go
67ee375f47eeca676e170c22a6d1812d  ./json/stream.go
1277e282f6bf1c91d83f726aae7be95c  ./json/tables.go
e645ab9ef4ac5a16991a8b3905de6bf3  ./json/tags.go
fa505e819de58a7291c803df6f5d0385  ./json/verify_off.go
85a5297471472eb63e988a33f473634b  ./json/verify_on.go
452041219f0bed5be171bf76890c1d4a  ./keybinding_hints.go
fb5bf9048ee1b1f9128908e31f859abd  ./kit/bordered_loader.go
85a0464284a33750db8bc3fddd30dbf1  ./kit/branch_summary.go
8789670546fe38467b24cf5200953c14  ./kit/cancellable_loader.go
e4f5d5022b787c15014984091fd68417  ./kit/compaction_summary.go
339551be5e0134f926807f7cc06b4688  ./kit/conversation.go
7684cbd12238edab0c6f94bdb12279d6  ./kit/custom_message.go
3b726e065df4c1056967c1181aad450d  ./kit/editor.go
40140542a858ae0e75745659f35eaa2a  ./kit/event.go
42718418a997457875c962befd77045c  ./kit/input.go
d94b8854fca62e198cadd0f5c046f0ec  ./kit/kit.go
62f8d550b8459f561e2c83c0f5052039  ./kit/mouse_region.go
3eeff49dcc676c2022a63962a07dddce  ./kit/scroll_view.go
f4a2c110d17412ea9341130445f2e977  ./kit/skill_invocation.go
384f7e0b97de00f7fd73bb27d553dd35  ./kit/widget.go
4421611322d55ecd27a457c3a2f19791  ./language.go
f96605bcf0beeadbac530f890dcbe64e  ./LICENSE
828daa940721ae461f086e51a414417a  ./llm_convert.go
2aeb569bc8e486bcdf7241a46de84b29  ./login.go
4c5024358071e1f6e448dc770d26dee0  ./lossless_json.go
ad6742b03a4e507b82a71ac5fc356a3e  ./mcp_servers.go
122fcaa57566fbf20e56504520d34810  ./model_registry.go
55ab959f4dacd93dceb0817020204291  ./model_runtime.go
d54154addd1cb6045b9a5b3388d9bb41  ./model_stream_callbacks.go
12f4fbbf44d692ffea2f3ab267767ed0  ./model_types.go
9eebea7c5dac3d90ce890c90e6bc8c05  ./mouse.go
4b0b376c9d598b8870c9b27d58c18ce2  ./oauth_credentials.go
1c10aa1e68eadea7a9334824927240d6  ./oauth.go
a95703ab224e6795021d8acdbcbf1191  ./optional.go
0112c064027bd5cc1a127df160358d58  ./ordered.go
d0e69899db21c8f7319b9a7194cf857c  ./ordered_json.go
dbf6ad301bef5342c8bb86a7035ae017  ./overlay_handle.go
d730cbaadd0f24e4661636dc955fb74e  ./overlay_width.go
ea90dbc29be49c1d48e7d602b3a5f19e  ./package_manager.go
778011151f43ef1ea68d3c4ac428eec1  ./pi_ai_models.go
4747f72e97f80fc4f29fe9895dc923d9  ./pi_ai_text.go
12c2e2312410bbfe324874fd89285d9d  ./.pig-sdk-version
d257d6964d809b3e32ebfa0433749ea1  ./project_trust_store.go
7add9abe7afc2b2f1aee1efa4c1dcf79  ./prompt_sections.go
b3af103a11629699b8fb56cf3479da1a  ./protocol.go
516794d951d6c2584eeabb66d4ef5f33  ./provider_dispatch.go
82e74d150ece9a101c52e64ce5ea3b52  ./provider.go
797541c86a5741ded303256c59500b94  ./provider_proxy.go
b8fea55139355783144b653bf55455cc  ./provider_stream.go
21a3fbf6500dbdda2abc1767d6b16e77  ./replaced_session.go
21bc39681f2a8877e375e8b294bccfe5  ./request_lifetime.go
c2a945280360556225f29a5c12147afd  ./resource_loader.go
eb0ab0b1946da748da9db372db0398a8  ./run_signal.go
6c2ff199966bf58b7c153634e7307ca0  ./scoped_models.go
7fff55ff2fec5295d0dfa1663952c1d8  ./serialize_conversation.go
47edc1358eeace363c05bf022e60c19e  ./session_entry_messages.go
f434474bee4bb438f20d7ce0967b46fd  ./session_graph.go
3344c54cd3e2ff42fc660d2e5ead3ce7  ./session_manager_class.go
6b00e96b4329a7d60b7444b7d7fd3b37  ./session_manager.go
5e85a685f9c4ad7a106b882d92978614  ./session_mirror.go
cba5b7c8f004c97c519ad9569970a4c2  ./session_skill_helpers.go
2a0ddfd6d5d618bb3dddaeb1b2d2a2fb  ./settings.go
73e552460de285d484ec7b9a8edc227c  ./settings_manager.go
8bdbcbc25ba1b6ef9c79c39231ee8740  ./setup_session.go
b655da6d9de5fa37018498552d2a3eef  ./state_feed.go
7727dcf64ca71097330403142b357e80  ./stored_credential.go
c5277dc702c8962bc77e301d7f84de64  ./surface.go
2cdcf7a3d183a22a650010fa7d1398af  ./system_prompt_options.go
488d6d43b10d40d1d0cc3f55d59812a6  ./theme_helpers.go
a20f1e35311e06699c4d3ae9c4de9722  ./tool_call_context.go
ec7bed51a64844558a366dc04e18ab4b  ./tool_call_guard.go
07d55501f07711b98dd4c0c9ad3015bb  ./tool_call_input_order.go
a3b5b8df8b53e3f02c54a1a909710436  ./tool_definition.go
950cd723a0829146ded0fd2906652af7  ./tool_orchestration.go
db0698205cbdfb7f06104bef30e00fe2  ./tool_renderer_resolvers.go
2fa625d6b543b229bc68743472c62e68  ./tool_renderers.go
c74d1348bc7ffabffcc8d01bb7ca77cc  ./tool_result_guards.go
075c261a213b26a540472970a1add34d  ./tool_start_order.go
73592d9ec08a78b3037d309d1bb7aa97  ./truncate.go
4dd0ca29e3a347c6981dbf15775dc283  ./tui/colors.go
3d0fb055c320b352d88e96bb005d645c  ./tui/doc.go
c9b599641956ae0dab080cb12de26c63  ./tui/fuzzy.go
338b3141bcdad9a986a689db8ae5cdf8  ./tui/internal/engine/alt_screen_keybindings.go
2aa83a6002b27d9bc81a0540d47895b2  ./tui/internal/engine/colors.go
f3be6af928233d49e6bf7a4dca29eeb1  ./tui/internal/engine/fuzzy.go
89aa74159c2cc224b78a64ebb7e4e60a  ./tui/internal/engine/keybindings.go
1a64b4daa4aba08fe799eead9773bb6a  ./tui/internal/engine/key_helper.go
618184c6a2d78ec842255ec03109804b  ./tui/internal/engine/key_match.go
c59879ba2a92e309775229766e4a9105  ./tui/internal/engine/key_parse.go
730183a6bed5fca48b301895a3a3875c  ./tui/internal/engine/keys_decode.go
d68f08aa6da96875a8af688844c970c4  ./tui/internal/engine/kitty_state.go
20106c8558677605b0f2707cfc00cd61  ./tui/internal/engine/oklab.go
5edf18ca3a9f8ab3078982ef4de836f4  ./tui/internal/engine/rgb.go
1ccb675af7603aaef4d376e9bbdcf5fa  ./tui/internal/jsnumber/number.go
f0dfd1b0102151579899370e188d18d3  ./tui/internal/jsstring/case.go
f0f4b94806c8d333993c5ae15104e9a3  ./tui/internal/jsstring/codepoint.go
36105474219892c2551cdcc3ff679723  ./tui/internal/jsstring/slice.go
e3dffd220d53f1053a159a0a9e5ff5ef  ./tui/internal/jsstring/space.go
ea8632545f17eacad021d28e3790cb07  ./tui/internal/jsstring/utf16.go
d9b8298d24daea579872ceb2e5b583d8  ./tui/internal/lazyregexp/lazyregexp.go
7812f952ed92af6af651c3d9b8bb3d82  ./tui/internal/widthx/cursor_marker.go
cfe00303fbfde3acf39959866444e92a  ./tui/internal/widthx/frame.go
9ae428b68a1ec67e59d063e1464ef078  ./tui/internal/widthx/grapheme_iter.go
2305fbb205ee979933744b8a6dce53d3  ./tui/internal/widthx/grapheme_range.go
44d200b16d0f5fbc9e26776222fe1196  ./tui/internal/widthx/line_resets.go
6035cf8688144c20ef798fd87b3f1130  ./tui/internal/widthx/pi_width.go
195201c3134aa5af865324392190fca4  ./tui/internal/widthx/slice.go
36a354f3131fc4fc805544cd7355bb7f  ./tui/internal/widthx/tracker.go
cd7cafd96d6867a797527144b2a3b846  ./tui/internal/widthx/truncate.go
269c0c7dd09b78c11f3016010b2380b8  ./tui/internal/widthx/unicode_tables.go
64d62af3a2402bafcbafee454bac8648  ./tui/internal/widthx/widthx.go
7d78d2a7a4099c268051058ffeab3ec5  ./tui/internal/widthx/wrap.go
77d3b3a6d9dcb402cd7941436d4ccb47  ./tui/keybindings.go
5defe6e8637b890bc959e53e91bdb8f0  ./tui/keys.go
2394e7e9f89c4c0f0cf1fd75e266e7db  ./tui_state.go
1b6bd810ed34f91c1a22a30e5de61d77  ./tui/terminal.go
10e798b8e9a95e8d21d6497dc8654627  ./tui/text.go
4e50013bc9f9c72eda9d87eafde87af2  ./ui_theme.go
825e2de205359a4a445099d55833b3fa  ./upgrade/diagnostics.go
b690dbefbd85a386ce91b3e9eaccad35  ./upgrade/diff.go
335f13b3d57aa951086ea64c92a13fd5  ./upgrade/getters.go
a6dedd29bdb7c42876d43a811004b605  ./upgrade/helpers.go
ffce0c2441ca1a51933b1ff950e9c896  ./upgrade/load.go
e416af3b2d420e2fdf90b5d232d7e39e  ./upgrade/options.go
0a46127b6481209ff4a20f53f6915e4e  ./upgrade/plan.go
e80ae5211cc3138f68a02dde5d14b0b3  ./upgrade/rules.go
f83d131bec7b9f1ab99fbb953353cbf1  ./upgrade/system_prompt.go
0da99dd89c15765263531600f61364e9  ./user_bash.go
a565c75679c402e3b64efa3a0e361b60  ./view.go
745e6a1a275d7ff7af2c882110eb562c  ./virtual_models.go
1a2cb1f166eb6549a5a94bc3bc51489f  ./visual_truncate.go
```
