# subtitle-matcher

**番号字幕自动匹配下载器** —— 扫描本地媒体库目录，自动从 [subtitlecat.com](https://www.subtitlecat.com/) 搜索并下载中文字幕。

单个 `.exe` 文件，无任何运行时依赖，双击即用。

---

## 功能

- **自选目录** —— 弹出 Windows 原生文件夹选择框，任意磁盘、任意路径都能选
- **列出目录列表** —— 自动扫描所选目录下的所有番号文件夹并编号展示
- **自动跳过** —— 文件夹内已存在字幕文件（`.srt` / `.ass` / `.ssa` / `.sub` / `.vtt` / `.smi` / `.idx` / `.ttml` / `.sbv`）则跳过
- **按大小取最大** —— 搜索结果按 `SIZE` 字段从大到小排序，优先下载体积最大的
- **简体优先** —— 语言优先 `Chinese (Simplified)`，其次候补 `Chinese (Traditional)`，两者都没有则跳过
- **自动命名** —— 字幕保存为「文件夹同名.srt」，直接适配 Jellyfin / Emby / Plex

## 使用

### 双击运行

直接双击 `SubtitleCatMatcher.exe`，弹出文件夹选择框，选好目录后自动开始处理。

### 命令行

```bash
SubtitleCatMatcher.exe                         # 弹出文件夹选择框
SubtitleCatMatcher.exe -d "D:\影片"             # 直接指定目录
SubtitleCatMatcher.exe -d "D:\影片" -all        # 不筛番号，处理全部子文件夹
SubtitleCatMatcher.exe -d "D:\影片" -v          # 输出每条字幕的下载地址
SubtitleCatMatcher.exe -d "D:\影片" -delay 2s   # 放慢请求间隔
SubtitleCatMatcher.exe -h                      # 查看全部参数
```

| 参数 | 说明 |
|---|---|
| `-d <目录>` | 直接指定目录，跳过选择框 |
| `-delay <时长>` | 每次请求之间的间隔，默认 `700ms` |
| `-all` | 不过滤番号，处理所有子文件夹 |
| `-v` | 输出详细下载地址 |
| `-q` | 安静模式，只输出结果 |

## 运行效果

```
目标目录：D:\影片

共发现 4 个文件夹：
   1. SNOS-115
   2. SNOS-149
   3. SNOS-172
   4. SNOS-245

[1/4] SNOS-115   ✔ Chinese (Simplified) | 来源《SNOS-115 jp》 105 KB | 99.1 KB
[2/4] SNOS-149   ✔ Chinese (Simplified) | 来源《SNOS-149》 9 KB | 7.3 KB
[3/4] SNOS-172   ✔ Chinese (Simplified) | 来源《489155.com@SNOS-172-U.ja.whisperjav》 64 KB | 57.9 KB
[4/4] SNOS-245   ✔ Chinese (Simplified) | 来源《SNOS-245》 7 KB | 6.7 KB

──────────── 汇总 ────────────
  成功下载: 4
  跳过:     0
  合计:     4
```

## 匹配规则

1. 用文件夹名作为关键字在 subtitlecat.com 搜索
2. 解析结果列表，按 `SIZE` 字段**从大到小**排序
3. 逐个打开详情页，查找可下载的中文字幕
   - 先找 `Chinese (Simplified)`
   - 没有再找 `Chinese (Traditional)`
   - 都没有就换下一条结果
4. 找到即下载，保存为 `<文件夹名>.srt`

### 防误匹配

实测中发现，把任意目录名丢去搜索会匹配到完全无关的字幕（例如 `subtitle-matcher` 曾匹配到某部毫不相干的片子）。因此加了两道防线：

- **番号格式过滤** —— 默认只处理符合番号格式的文件夹，正则为
  `^\d{0,5}[A-Z]{2,10}[-_ ]?\d{2,6}`，覆盖 `SNOS-115`、`SSIS-001`、`259LUXU-1234`、`HEYZO-1234` 等常见写法。
  需要处理全部文件夹时加 `-all`。
- **标题相关性校验** —— 搜索结果标题归一化（去符号、转大写）后必须真的包含该番号，否则丢弃该条结果。

## 编译

需要 Go 1.20+。

```bash
go build -trimpath -ldflags "-s -w" -o SubtitleCatMatcher.exe .
```

仓库中已包含 `rsrc.syso`（图标 + manifest 资源），`go build` 会自动链接，图标开箱即用。

### 重新生成图标资源

如果想换图标：

```bash
# 1. 任意图片 → 多尺寸 ICO（需要 Python + Pillow）
python -c "
from PIL import Image
im = Image.open('cat.webp').convert('RGBA')
w, h = im.size; s = min(w, h)
im = im.crop(((w-s)//2, (h-s)//2, (w-s)//2+s, (h-s)//2+s))
im.save('app.ico', format='ICO',
        sizes=[(16,16),(24,24),(32,32),(48,48),(64,64),(128,128),(256,256)])
"

# 2. 生成资源文件（需要 rsrc：go install github.com/akavel/rsrc@latest）
rsrc -ico app.ico -manifest app.manifest -o rsrc.syso -arch amd64

# 3. 重新编译
go build -trimpath -ldflags "-s -w" -o SubtitleCatMatcher.exe .
```

## 测试

```bash
go test -v ./...
```

覆盖了语言优先级（简体优先 / 繁体候补 / 无下载链接）、`SIZE` 解析与排序、番号格式识别等核心逻辑。

## 实现说明

- **纯标准库** —— 除了 Windows 系统 DLL 的 syscall 调用外，没有任何第三方依赖，`go.mod` 里没有 require
- **原生文件夹选择框** —— 直接调 `shell32.SHBrowseForFolderW`，不走 cgo
- **控制台彩色输出** —— 通过 `SetConsoleMode` 打开 ANSI 转义支持，失败时静默降级
- **限速与重试** —— 默认 700ms 请求间隔、3 次重试，避免给站点造成压力

## 声明

本工具仅用于下载用户已有媒体文件所对应的字幕，不提供、不存储、不分发任何音视频内容。
请遵守当地法律法规及目标站点的服务条款，合理使用。

## 许可

未指定。
