// paperplane-plugin-android 是 android 插件的独立可执行入口。
// 带 __run 参数启动时是运行器（见 plugin/runner.go），否则是插件进程。
package main

import (
	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"

	android "github.com/lemon-paperplane/lemon-paperplane-plugin-android/plugin"
)

func main() { sdk.Serve(android.New()) }
