# Java Navigation for Lithe

为 [Lithe](https://github.com/1lck/Lithe-IDEA) 提供**不依赖 JDT LS 的轻量 Java 跳转**：跳转到定义、查找引用和文档结构。
插件自带一个用 Go 写的小型语言服务器，不需要 JDK 运行时、不导入 Maven/Gradle 构建，打开项目后第一次跳转约 0.2 秒就能用，
常驻内存几十 MB（JDT LS 通常要 3–5 GB，首次导入几十秒到几分钟）。
这是一个**社区插件**，按插件契约 v0 编写。

> English: lightweight Java navigation for Lithe without JDT LS — go to definition, find references and the
> document outline, served by a small Go language server that keeps an index of declarations only. No JDK
> runtime, no build import, tens of MiB of memory.

> **预发布**：按插件契约 v0（`schemaVersion: 0`、`pluginAPIVersion: 0`）编写，只能装进新版 Lithe 桌面端
> （`desktop-v*` 预发布），旧版 macOS Lithe 无法安装。

## 功能

| 功能 | 说明 |
| --- | --- |
| 跳转到定义 | 类型、方法、构造器、字段、枚举常量、局部变量、参数、类型参数、注解及其属性；按导入、同包、通配导入、嵌套、继承链和调用参数个数解析，支持调用链 `a.b().c` |
| Lombok | `@Data`/`@Getter`/`@Setter` 生成的访问器跳到字段，`@Builder`、`@Slf4j`、构造器注解跳到注解本身（与 JDT LS 一致） |
| 跳进 JDK | 项目里找不到的名字到本机 JDK 的 `src.zip` 里找，`String`、`List`、`hashCode()` 等都能跳，并能在 JDK 源码里继续跳 |
| 查找引用 | 只列出能跳回这个声明的出现，所以列表和在任一行上跳转的结果一致；`package`/`import` 不算引用 |
| 文档结构 | 类型、成员和嵌套类型的大纲（`textDocument/documentSymbol`） |
| 未保存的修改 | 打开的文件按编辑器里的内容索引，未保存的新方法也能从其他文件跳过去 |

## 怎么工作

- 索引只保留**声明**（类型、父类型、成员及其书写的类型、参数个数、Lombok 生成的成员），不保留语法树，也不做类型绑定。
- 第一次跳转时才建索引：在 Git 仓库里用 `git ls-files` 列出已跟踪和未跟踪的文件（跳过 `.gitignore` 忽略的），
  否则遍历目录并跳过隐藏目录与 `target`、`build`、`out`、`bin`、`node_modules`。超过 2 MiB 的文件跳过。
- 之后每次查询如果距上次扫描超过 2 秒，就按文件大小和修改时间增量重扫，其他工具改的文件也能反映出来。
- JDK：按 `JAVA_HOME` → 系统默认 JDK（macOS 的 `/usr/libexec/java_home`，其他系统 `PATH` 上的 `java`）→ 常见安装目录
  （含 IntelliJ 的 `~/.jdks`、SDKMAN!）的顺序找第一个带 `src.zip` 的 JDK。只预先列出 zip 目录，某个类第一次被查到时才解析。
  跳进 JDK 时把那一个源码文件解压成只读副本，放在 Lithe 为插件分配的状态目录（`${stateDir}`）里，卸载插件时一起删除；
  不写插件包或应用安装目录。

解析规则来自 [Viff](https://github.com/1lck/viff)（MIT），见 `internal/codenav/UPSTREAM.md` 和 `THIRD_PARTY_NOTICES.md`。

## 实测（Apple M4，macOS，JDK 17）

在 `src/main/java` 里随机取 1500 个标识符（不含关键字、字符串里的词、`package`/`import` 行），两个种子：

| 项目 | Java 文件 | 能跳转的比例 | 其中跳进 JDK | 单次 p50 / p99 | 第一次跳转（含建索引） | 服务器常驻内存 |
| --- | --- | --- | --- | --- | --- | --- |
| Nacos | 2886 | 93.9% / 94.9% | 333 / 326 | 0.07 ms / 0.8–1.0 ms | 0.12 s | 约 74–76 MiB |
| ruoyi-vue-pro | 6355 | 54.3% / 53.3% | 198 / 207 | 0.06 ms / 0.5–0.6 ms | 0.17 s | 约 75–85 MiB |

查找引用 20 次，p50 约 56 ms，最慢 0.13 s。

宿主联调：用 Lithe 桌面端固定的 Rust Core（`47c9859`）校验 `plugin.json`、规划 `packaged` 启动、经 Core 启动语言服务器，
会话进入 `ready` 并声明 `definition`、`references`、`documentSymbols`，三种请求都返回正确位置；跳进 JDK 时只在 `${stateDir}`
写入解压副本，插件包文件的 SHA-256 前后一致。ruoyi-vue-pro 跳不了的几乎全是依赖库的注解和它们的属性：
Swagger 的 `@Schema(description, example, requiredMode)`、Lombok 的 `@Data`、校验注解 `@NotNull`、EasyExcel、Spring 注解等，
见下面的“限制”。

## 限制

- **不读依赖库**：Spring、MyBatis、Lombok、Swagger 等依赖里的声明找不到（返回“没有定义”，不会跳错）。
  计划：从 Maven/Gradle 插件取 classpath，按需读取 `sources.jar`，没有源码时显示只有签名的存根。
- **没有类型推断**：重载只按参数个数选择；lambda 参数的类型和泛型替换后的返回类型推不出来，
  例如 `list.get(0).getName()` 里的 `getName`、`users.stream().map(u -> u.getName())` 里的 `getName`；
  以字面量为限定符的调用（`"".trim()`）也推不出类型。
- **只认一个 JDK**：用的是上面顺序找到的 JDK，不一定是项目编译用的版本。
- **引用不含重写和实现**：查找方法的引用不包括子类重写的方法和通过接口调用到的其他声明。
- **生成代码**：注解处理器生成到 `target/generated-sources` 的类通常被 `.gitignore` 忽略，不进索引。
- **和 Java Support（JDT LS）同时安装**：两个插件都为 Java 声明语言服务器，宿主怎么分配请求还没有定义，暂时不要同时启用。

## 开发

需要 Go 1.27.3 和 Node.js 22（只用于打包）。

```sh
go test ./...                     # 单元测试和端到端协议测试
go test -race ./...
node scripts/package.mjs          # 交叉编译四个平台并打包到 dist/
```

`dist/` 里每个平台一个 zip（`darwin-arm64`、`darwin-x64`、`windows-x64`、`windows-arm64`），包内只有
`plugin.json`、`bin/java-navigation`（Windows 是 `bin/java-navigation.exe`，对应平台包的 `plugin.json` 也写这个路径）、
`LICENSE` 和 `THIRD_PARTY_NOTICES.md`。

语言服务器可以单独运行调试：

```sh
go run ./cmd/java-navigation --state-dir /tmp/jn-state   # 通过 stdin/stdout 说 LSP，日志写 stderr
go run ./cmd/java-navigation --version
```

## 发布

推送与 `plugin.json` 版本一致的 tag `v<version>`，`release.yml` 会打包并创建 GitHub Release。
`PRERELEASE_TAGS` 里列出的 tag 发布为预发布，Lithe Store 的自动追踪会跳过，需要手工提交收录。

## 许可

Apache License 2.0，见 `LICENSE`。`internal/codenav` 等来自 Viff 的代码按 MIT 许可，见 `THIRD_PARTY_NOTICES.md`。
