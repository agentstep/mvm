// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "mvm-vz",
    platforms: [.macOS(.v15)],
    dependencies: [
        .package(url: "https://github.com/apple/swift-argument-parser.git", from: "1.3.0"),
    ],
    targets: [
        .executableTarget(
            name: "mvm-vz",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
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
