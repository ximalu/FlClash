allprojects {
    repositories {
        val isCi = System.getenv("GITHUB_ACTIONS") == "true"
        if (isCi) {
            google()
            mavenCentral()
            maven { url = uri("https://jitpack.io") }
        } else {
            // 本地 maven：fastdev-jsruntimes 等镜像拉不到的 artifact（2026-09-09）
            maven { url = uri("file:///home/ximalu/.m2-local") }
            maven { url = uri("https://mirrors.cloud.tencent.com/nexus/repository/maven-google/") }
            maven { url = uri("https://maven.aliyun.com/repository/google") }
            maven { url = uri("https://mirrors.cloud.tencent.com/nexus/repository/maven-public/") }
            maven { url = uri("https://maven.aliyun.com/repository/central") }
            maven { url = uri("https://maven.aliyun.com/repository/gradle-plugin") }
            maven { url = uri("https://mirrors.cloud.tencent.com/nexus/repository/gradle-plugins/") }
            // com.github.* 坐标（fastdev-jsruntimes 等）走 JitPack（2026-09-09）
            maven { url = uri("https://jitpack.io") }
        }
    }
}

val newBuildDir: Directory =
    rootProject.layout.buildDirectory
        .dir("../../build")
        .get()
rootProject.layout.buildDirectory.value(newBuildDir)

subprojects {
    val newSubprojectBuildDir: Directory = newBuildDir.dir(project.name)
    project.layout.buildDirectory.value(newSubprojectBuildDir)
    project.evaluationDependsOn(":app")
}

gradle.projectsEvaluated {
    allprojects {
        tasks.withType<JavaCompile>().configureEach {
            sourceCompatibility = JavaVersion.VERSION_17.toString()
            targetCompatibility = JavaVersion.VERSION_17.toString()
        }
        tasks.withType<org.jetbrains.kotlin.gradle.tasks.KotlinCompile>().configureEach {
            compilerOptions {
                jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
            }
        }
    }
}

tasks.register<Delete>("clean") {
    delete(rootProject.layout.buildDirectory)
}
