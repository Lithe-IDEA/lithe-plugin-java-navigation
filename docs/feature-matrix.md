# 功能验证矩阵

## Linux 安装包

| 能力 | Linux ARM64 | Linux x64 | 验证入口 |
| --- | --- | --- | --- |
| 构建与安装包 | [已实现并通过原生 CI](https://github.com/Lithe-IDEA/lithe-plugin-java-navigation/actions/runs/43619383227) | [已实现并通过原生 CI](https://github.com/Lithe-IDEA/lithe-plugin-java-navigation/actions/runs/43619383227) | `.github/workflows/ci.yml`、[资源说明](linux-packaging.md) |
| 真实工具/语言服务器流程 | [已通过原生 CI](https://github.com/Lithe-IDEA/lithe-plugin-java-navigation/actions/runs/43619383227) | [已通过原生 CI](https://github.com/Lithe-IDEA/lithe-plugin-java-navigation/actions/runs/43619383227) | `scripts/package/` 和现有集成测试 |

跨架构本地包检查通过不等于原生工作流验收。既有平台验证记录保持不变。
