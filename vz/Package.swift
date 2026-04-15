// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "mvm-vz",
    platforms: [.macOS(.v15)],
    dependencies: [
        .package(url: "https://github.com/apple/swift-argument-parser.git", from: "1.3.0"),
    ],
    targets: [
        // C shim for SCM_RIGHTS file-descriptor passing. The CMSG macros
        // don't bridge cleanly into Swift, so the sendmsg-with-ancillary
        // dance lives in fd_util.c. Public header exposes one function.
        .target(
            name: "MvmVZShim",
            path: "Sources/MvmVZShim",
            publicHeadersPath: "include"
        ),

        .executableTarget(
            name: "mvm-vz",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
                "MvmVZShim",
            ],
            path: "Sources/mvm-vz",
            swiftSettings: [
                .unsafeFlags(["-parse-as-library"]),
            ],
            linkerSettings: [
                .linkedFramework("Virtualization"),
            ]
        ),
    ]
)
