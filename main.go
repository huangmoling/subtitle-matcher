//go:build windows

// 番号字幕匹配下载器（SubtitleCat Matcher）
//
// 图形界面版：
//  1. 点「浏览…」任选磁盘上的字幕库目录
//  2. 递归扫描目录下所有子目录里的视频文件（含 .strm 流占位文件）
//  3. 并行到 subtitlecat.com / aiyi1.com / javzimu.com 搜索
//  4. 哪个站有结果就用哪个；语言优先简体，其次繁体，同语言取文件最大的
//  5. 下载后命名为「视频同名.srt」，视频已有同名字幕则自动跳过
//
// 双击即用，不需要命令行。
//
// 调试用（保留但不作为正常使用方式）：
//
//	SubtitleCatMatcher.exe -cli [目录]     走控制台流程
//	SubtitleCatMatcher.exe -cli -all 目录  不过滤番号
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const appTitle = "番号字幕匹配下载器 v2.1"

func main() {
	args := os.Args[1:]
	if hasArg(args, "-cli") || hasArg(args, "--cli") {
		os.Exit(runCLI(args))
	}

	// 支持 SubtitleCatMatcher.exe "D:\影片" 直接带目录进界面
	initial := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			initial = a
			break
		}
	}
	os.Exit(runGUI(initial))
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

const cliUsage = `番号字幕匹配下载器（控制台调试模式）

用法:
  SubtitleCatMatcher.exe -cli [选项] [目录]

选项:
  -all          不过滤番号，处理所有视频文件
  -raw          原样保存字幕，不修正时间轴格式
  -jobs N       同时处理的视频文件数（默认 3）
  -h            显示本帮助

不带 -cli 直接运行会打开图形界面。
`

// runCLI 控制台流程，仅用于开发调试与自动化验证。
func runCLI(args []string) int {
	root := ""
	onlyCodes := true
	normalize := true
	jobs := 3

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-cli", "--cli":
		case "-all":
			onlyCodes = false
		case "-raw":
			normalize = false
		case "-jobs":
			if i+1 < len(args) {
				i++
				if n, err := atoi(args[i]); err == nil && n > 0 {
					jobs = n
				}
			}
		case "-h", "--help":
			fmt.Print(cliUsage)
			return 0
		default:
			if !strings.HasPrefix(a, "-") {
				root = a
			}
		}
	}

	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Println("无法确定当前目录:", err)
			return 1
		}
		root = wd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		fmt.Printf("目录不存在或不可访问: %s\n", abs)
		return 1
	}

	cfg := config{OnlyCodes: onlyCodes, Normalize: normalize, SkipExisting: true, Concurrency: jobs}

	targets, ignored, err := scanVideos(abs, cfg.OnlyCodes)
	if err != nil {
		fmt.Println("读取目录失败:", err)
		return 1
	}
	if len(ignored) > 0 {
		fmt.Printf("已忽略 %d 个不像番号的视频\n", len(ignored))
	}
	if len(targets) == 0 {
		fmt.Println("没有找到需要配字幕的视频文件")
		return 0
	}

	fmt.Printf("目录: %s\n共 %d 个视频文件，并发 %d\n\n", abs, len(targets), cfg.Concurrency)

	var mu sync.Mutex
	printed := map[int]string{}

	sum := runAll(context.Background(), targets, cfg, func(p progress) {
		if p.Status == stSearching {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		line := fmt.Sprintf("[%d/%d] %-22s %-9s %s",
			p.Index+1, len(targets), targets[p.Index].Name,
			p.Status.Text(), p.Detail)
		if printed[p.Index] == line {
			return
		}
		printed[p.Index] = line
		fmt.Println(line)
	})

	fmt.Printf("\n──────── 汇总 ────────\n成功 %d ｜ 跳过 %d ｜ 失败 %d ｜ 合计 %d\n",
		sum.Done, sum.Skipped, sum.Failed, sum.Total)

	if sum.Failed > 0 {
		return 2
	}
	return 0
}

func atoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("空字符串")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("非数字")
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}
