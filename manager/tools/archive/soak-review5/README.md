# 历史 soak-review5 环境脚本

这里的三份脚本写死了早期测试目录、8081/4842 端口、测试组和来源 ID。它们已从 `manager/tools/` 的常用入口移出，内容原样保留，以便追溯旧验收。

不适用于当前 4840/4841/8087 部署；尤其 `remote-route-switch.py` 会修改默认来源，不要直接运行。当前验证要求见 [验收文档](../../../docs/validation/validation.md)。
