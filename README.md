# Java Navigation for Lithe

为 [Lithe](https://github.com/1lck/Lithe-IDEA) 提供**不依赖 JDT LS 的轻量 Java 跳转**：跳转到定义、查找引用和文档结构，
能跳进 JDK 和 Maven 依赖库。插件自带一个用 Go 写的小型语言服务器，不需要 JDK 运行时、不运行 Maven，
打开项目后第一次跳转约 0.2–0.5 秒就能用，常驻内存 60–90 MiB（同一个项目 JDT LS 要 3 GB，导入要 1 分钟）。
这是一个**社区插件**，按插件契约 v0 编写。

> English: lightweight Java navigation for Lithe without JDT LS — go to definition, find references and the
> document outline, into the JDK and Maven dependency jars, served by a small Go language server that keeps
> an index of declarations only. No JDK runtime, no build import, 60–90 MiB of memory.

> **预发布**：按插件契约 v0（`schemaVersion: 0`、`pluginAPIVersion: 0`）编写，只能装进新版 Lithe 桌面端
> （`desktop-v*` 预发布），旧版 macOS Lithe 无法安装。

## 功能

| 功能 | 说明 |
| --- | --- |
| 跳转到定义 | 类型、方法、构造器、字段、枚举常量、局部变量、参数、类型参数、注解及其属性；按导入、同包、通配导入、嵌套、继承链、调用参数个数和实参类型解析，支持调用链 `a.b().c` 和 `Foo.<T>bar()` |
| Lombok | `@Data`/`@Getter`/`@Setter` 生成的访问器跳到字段，`@Builder`、`@Slf4j`、构造器注解跳到注解本身（与 JDT LS 一致） |
| 跳进 JDK | 项目里找不到的名字到本机 JDK 的 `src.zip` 里找，`String`、`List`、`hashCode()` 等都能跳，并能在 JDK 源码里继续跳 |
| 跳进依赖库 | Maven 项目的依赖（含传递依赖）从本地仓库读：下载过 `-sources.jar` 的打开源码，否则打开从 `.class` 生成的签名存根（类、成员类、字段、构造器、方法，参数名取自调试信息），并能在里面继续跳；`@Schema(description = ...)`、`log.info(...)`、`StrUtil.isEmpty(...)` 都能跳 |
| 老项目的 jar | 没有 Maven 的 Ant、Eclipse、MyEclipse 项目，读 `.classpath` 里列出的 jar（含 `M2_REPO` 变量和 `sourcepath`），以及 `lib/`、`libs/`、`WEB-INF/lib` 里的 jar |
| Eclipse 容器 | `.classpath` 里的服务器运行时容器（Tomcat、Jetty、WildFly 等）和 JUnit 容器换成本机已有的 jar，`HttpServletRequest`、`@WebServlet`、`@Test` 都能跳；没有 pom 的 `WEB-INF/web.xml` Web 项目同样补上 Servlet/JSP API |
| 生成代码 | 模块的 `target/generated-sources`（protobuf、注解处理器）和 Gradle 的 `build/generated` 也进索引，即使被 `.gitignore` 忽略 |
| 查找引用 | 只列出能跳回这个声明的出现，所以列表和在任一行上跳转的结果一致；`package`/`import` 不算引用 |
| 文档结构 | 类型、成员和嵌套类型的大纲（`textDocument/documentSymbol`） |
| 未保存的修改 | 打开的文件按编辑器里的内容索引，未保存的新方法也能从其他文件跳过去 |

## 怎么工作

- 索引只保留**声明**（类型、父类型、成员及其书写的类型、参数个数、Lombok 生成的成员），不保留语法树，也不做类型绑定。
- 第一次跳转时才建索引：在 Git 仓库里用 `git ls-files` 列出已跟踪和未跟踪的文件（跳过 `.gitignore` 忽略的），
  否则遍历目录并跳过隐藏目录与 `target`、`build`、`out`、`bin`、`node_modules`。超过 2 MiB 的文件跳过。
- 之后每次查询如果距上次扫描超过 2 秒，就按文件大小和修改时间增量重扫，其他工具改的文件也能反映出来。
- 依赖库：读工作区里的每个 `pom.xml` 和它的父 POM、导入的 BOM，再沿本地仓库（`~/.m2/settings.xml` 的
  `localRepository`，默认 `~/.m2/repository`）里的 POM 找传递依赖，规则和 Maven 一致：最近的声明优先、根模块的
  `dependencyManagement` 决定版本、`test`/`provided`/可选依赖不传递、`exclusions` 生效。不运行 Maven、不联网，
  本地仓库没有的 jar 跳过；版本是范围或引用了不认识的属性时取本地最高的版本。工作区自己的模块不当依赖。
  只预先列出每个 jar 的目录（Nacos 265 个 jar、4.4 万个类；ruoyi-vue-pro 824 个 jar、15 万个类，约 0.3 秒），
  某个类第一次被查到时才读它的源码或生成存根；跳进去时把那一个文件写成只读副本，放在 `${stateDir}/dependency-sources`，
  不写本地仓库。
- 工作区里的 jar：先按每个 Eclipse `.classpath` 的 `lib` 条目和 `M2_REPO` 变量条目的顺序，再加上路径往上三层内
  有 `lib` 或 `libs` 目录的 jar（`WEB-INF/lib`、`lib/optional` 都算；测试资源里的 jar、`gradle-wrapper.jar`、
  `target`/`build` 里的不算）；工作区根目录和每个 `build.xml`、`.classpath`、`.project` 所在目录下的 `lib`、
  `WEB-INF/lib`、`WebContent/WEB-INF/lib`、`WebRoot/WEB-INF/lib` 等即使被 `.gitignore` 忽略也会读。源码取
  `.classpath` 的 `sourcepath`，或旁边的 `-sources.jar`、`-src.zip`；源码包不是按包名放的（比如整个源码树打的 zip）就不用，改用存根。
  这些 jar 只读，不会被修改。
- Eclipse 容器（`kind="con"`）：服务器运行时容器按运行时名字判断包名（Tomcat 10+、Jetty 11+、WildFly 27+、
  GlassFish/Payara 6+、TomEE 9+ 用 `jakarta.servlet`，更早的用 `javax.servlet`），先找本机装的服务器
  （`CATALINA_HOME`/`CATALINA_BASE`、Homebrew、`/opt`、`/usr/share`、Windows 的 `Program Files`、`~/apache-tomcat-*`）
  的 `lib` 里包名对得上的 `servlet-api.jar`、`jsp-api.jar`、`el-api.jar`，没有就从本地 Maven 仓库取包名对得上的最新
  Servlet API 和 JSP API；`JUNIT_CONTAINER/3`、`/4`、`/5` 取本地仓库里对应大版本的最新 JUnit（4 加上 hamcrest-core）。
  项目自己已经带了 Servlet 或 JUnit 的 jar 时不再补。没有 `pom.xml`/Gradle 构建文件、但有 `WEB-INF/web.xml` 的
  项目按 Servlet 容器处理（web.xml 用 Jakarta EE 命名空间时取 `jakarta.servlet`）。不联网下载，本机都没有就跳过。
- `pom.xml`、`.classpath` 或上面任一个 jar 变了、或距上次读取超过 5 分钟，下一次扫描重新读取依赖。
- JDK：按 `JAVA_HOME` → 系统默认 JDK（macOS 的 `/usr/libexec/java_home`，其他系统 `PATH` 上的 `java`）→ 常见安装目录
  （含 IntelliJ 的 `~/.jdks`、SDKMAN!）的顺序找第一个带 `src.zip` 的 JDK。只预先列出 zip 目录，某个类第一次被查到时才解析。
  跳进 JDK 时把那一个源码文件解压成只读副本，放在 Lithe 为插件分配的状态目录（`${stateDir}`）里，卸载插件时一起删除；
  不写插件包或应用安装目录。

解析规则来自 [Viff](https://github.com/1lck/viff)（MIT），见 `internal/codenav/UPSTREAM.md` 和 `THIRD_PARTY_NOTICES.md`。

## 实测（Apple M4，macOS，JDK 17）

用 `cmd/nav-bench` 在 `src/main/java` 里随机取 1500 个标识符（不含关键字、字符串里的词、`package`/`import` 行），
逐个请求跳转到定义，再对其中 20 个方法调用请求查找引用。Nacos 取 3 个种子，ruoyi-vue-pro 取 2 个：

| 项目 | 能跳转的比例 | 单次 p50 / p99 | 第一次跳转（含建索引、读依赖） | 查找引用 p50 / 最慢 | 内存（物理占用 / RSS） |
| --- | --- | --- | --- | --- | --- |
| Nacos | 98.7% / 98.2% / 99.3% | 0.12–0.18 ms / 1.7–3.3 ms | 0.24–0.42 s | 61–81 ms / 0.18 s | 62–69 MiB / 91–101 MiB |
| ruoyi-vue-pro | 98.4% / 98.5% | 0.11 ms / 2.2–2.7 ms | 0.46–0.50 s | 92–94 ms / 0.22 s | 91 MiB / 106–108 MiB |

物理占用是活动监视器显示的“内存”；RSS 还包括进程已经还给系统、系统还没回收的页。只看项目本身
（`--no-dependencies`），Nacos 跑完同样的请求约 47 MiB。

### 和 JDT LS 对比

同样的 3 组 Nacos 位置，同时问 JDT LS 1.60（Homebrew，默认参数，Maven 导入完成后）和本插件，比较跳到的文件和行
（跳进 JDK 或依赖库的按类比较，因为两边的 JDK 版本和源码不一定相同）：

| | 本插件 | JDT LS |
| --- | --- | --- |
| 能跳转的比例 | 98.7% / 98.2% / 99.3% | 98.8% / 98.5% / 99.5% |
| 两边跳到同一处 | 1494 / 1494 / 1493（共 1500） | |
| 启动到可用 | 第一次跳转 0.24–0.42 s | 59–61 s（Maven 导入，本地仓库已有缓存，不含下载） |
| 跳转 p50 / p99 | 0.12–0.18 ms / 1.7–3.3 ms | 3.0–3.9 ms / 20–39 ms，最慢 1.4 s |
| 查找引用 p50 / 最慢 | 61–81 ms / 0.18 s | 128–211 ms / 4.7 s |
| 内存（物理占用 / RSS） | 62–69 MiB / 91–101 MiB | 2.96–3.0 GB / 3.1–3.3 GB |

剩下的差异：方法引用 `Timestamp::compareTo`、`Resolver::resolve` 本插件跳到方法本身，JDT LS 跳到函数式接口；
其余是 JDT LS 能跳、本插件跳不了的泛型和 lambda（`list.get(i).split(...)`、`map.get(a).get(b)`、比较器里的
`o1.getPriority()`），以及两处要按泛型实参挑选的重载（`JsonUtils.toObj(String, ...)` 和 `toObj(byte[], ...)`）。

### 小众框架和老项目

每个项目 1500 个随机位置（种子 2），源码目录按项目实际布局取。跳到的目标全部逐个核对过，都落在该名字的声明行上：

| 项目 | 类型 | 能跳转的比例 | 跳转 p50 / p99 | 第一次跳转 | 内存（物理占用） |
| --- | --- | --- | --- | --- | --- |
| [JFinal](https://github.com/jfinal/jfinal) | 国产小众 Web 框架，Maven | 96.8% | 0.13 ms / 1.5 ms | 57 ms | 21 MiB |
| [Nutz](https://github.com/nutzam/nutz) | 国产小众框架，源码直接放 `src/` | 97.8% | 0.15 ms / 1.3 ms | 55 ms | 27 MiB |
| [Solon](https://github.com/opensolon/solon) | 国产小众框架，150 个模块 | 96.5% | 0.12 ms / 2.1 ms | 0.19 s | 36 MiB |
| [Apache Ant](https://github.com/apache/ant) | 2000 年起的老项目，`src/main/org` | 99.1% | 0.19 ms / 1.3 ms | 56 ms | 29 MiB |
| [Struts 1](https://github.com/apache/struts1) | 已归档的老框架，JSF/JSP | 97.5% | 0.16 ms / 1.8 ms | 44 ms | 22 MiB |
| [JForum 2](https://github.com/rafaelsteil/jforum2) | 已归档的 Ant 项目，jar 提交在 `WEB-INF/lib`，没有 pom | 99.3%（读工作区 jar 之前 95.9%） | 0.15 ms / 1.4 ms | 37 ms | 20 MiB |
| [javaee-blog](https://github.com/Lemonreds/javaee-blog) | Eclipse 动态 Web 项目，Servlet + JSP，Tomcat 7 运行时容器，没有 pom | 100%（解析容器之前 91.7%） | 0.08 ms / 0.94 ms | 34 ms | 17 MiB |
| [OpenCms](https://github.com/alkacon/opencms-core) | 2000 年起的 JSP CMS，Gradle | 94.1% | 0.20 ms / 2.0 ms | 0.13 s | 54 MiB |

剩下没跳到的主要是：依赖没在项目里声明或本地仓库没有（JFinal 的 Servlet 依赖在 pom 里被注释掉了，Struts 的
JSF 依赖没下载过，OpenCms 的 Gradle 依赖读不了）、数组的 `.length`（JDT LS 也不跳）、`java.util.Date` 这种全限定名里的包名段，以及 lambda 参数。

## 限制

- **依赖库只读 Maven 和工作区里的 jar**：Gradle 项目的依赖还读不了（Gradle 的生成代码可以）；Ant 的
  `build.xml` 里写在 `lib` 之外目录的 classpath 也读不了。只用本地仓库里已有的 jar，
  从没构建过、依赖还没下载的项目跳不进依赖库。没有 `-sources.jar` 时打开签名存根，没有方法体和注释；
  编译时没带调试信息的接口和抽象方法，参数名是 `arg0`、`arg1`。
- **没有泛型推断**：重载先按参数个数选，个数相同时按实参里写得出来的类型挑（字面量、强转、`new` 表达式、
  变量和字段声明的类型、能跟下去的调用链），都看不出时取第一个；lambda 参数的类型和泛型替换后的返回类型推不出来，
  例如 `list.get(0).getName()` 里的 `getName`、`users.stream().map(u -> u.getName())` 里的 `getName`；
  按泛型实参才能区分的重载也会取第一个。
- **`.jsp` 文件里不能跳转**：插件只处理 `.java` 文件，JSP 里的 `<% %>` 脚本、`<jsp:useBean>`、EL 表达式和标签库都不跳。
- **只认一个 JDK**：用的是上面顺序找到的 JDK，不一定是项目编译用的版本。
- **引用不含重写和实现**：查找方法的引用不包括子类重写的方法和通过接口调用到的其他声明。
- **和 Java Support（JDT LS）同时安装**：两个插件都为 Java 声明语言服务器，宿主怎么分配请求还没有定义，暂时不要同时启用。

## 开发

需要 Go 1.27.3 和 Node.js 22（只用于打包）。

```sh
go test ./...                     # 单元测试和端到端协议测试
go test -race ./...
node scripts/package.mjs          # 交叉编译四个平台并打包到 dist/
go run ./cmd/nav-bench -h         # 在真实项目上测命中率、延迟和内存（见文件开头的说明）
```

`internal/deps/testdata/fixture` 里的 jar 由同目录的 `build.sh` 用 javac 编出（需要 JDK 17+），测试只读提交的 jar；
改了 `internal/deps/stub.go` 的输出时，运行 `go test ./internal/deps -run Golden -update` 更新
`testdata/stubs`，并把 `stubFormat` 加一，让旧版本解压的副本不再被复用。

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
