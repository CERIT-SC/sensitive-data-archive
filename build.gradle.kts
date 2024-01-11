plugins {
    base
}

version = "1.1.0"

// Build
val goBuild = tasks.register("goBuild", Exec::class) {
    description = "Build the Go application"
    commandLine("go", "build", "-o", "build/")
}

tasks.named("build") {
    dependsOn(goBuild)
}

// Test
tasks.register("test", Exec::class) {
    group = "verification"
    description = "Test the Go application"
    commandLine("go", "test")
}

// Build Docker image
tasks.register("buildDockerImage", Exec::class) {
    group = "build"
    description = "Builds the Docker image for the Go application"
    commandLine("docker", "build", "-t", "mq-interceptor", ".")
}

// Cleanup
val goClean = tasks.register("goClean", Exec::class) {
    description = "Deletes the build directory"
    commandLine("go", "clean")
}

tasks.named("clean") {
    dependsOn(goClean)
}