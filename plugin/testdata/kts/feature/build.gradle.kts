plugins {
    alias(libs.plugins.android.library)
}
android {
    namespace = "com.example.feature"
    compileSdk = libs.versions.compile.sdk.get().toInt()
    defaultConfig { minSdk { version = release(23) } }
}
