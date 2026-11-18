plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
}

android {
    namespace = "com.example.kts"
    compileSdk {
        version = release(36) {
            minorApiLevel = 1
        }
    }
    defaultConfig {
        applicationId = "com.example.kts"
        minSdk = libs.versions.minSdk.get().toInt()
        targetSdk = 36
        versionCode = 1
    }
    buildTypes {
        getByName("debug") {
            applicationIdSuffix = ".dev"
        }
        release {
            isMinifyEnabled = false
        }
        create("benchmark") {
            initWith(getByName("release"))
        }
    }
}
