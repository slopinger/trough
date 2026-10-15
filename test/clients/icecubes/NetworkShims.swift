import Foundation
#if canImport(FoundationNetworking)
@_exported import FoundationNetworking
#endif

// OSLog and os's lock are Apple-only.
struct Logger: Sendable {
  init(subsystem: String, category: String) {}
  enum Level { case info, debug, error, fault }
  func log(level: Level, _ s: @autoclosure () -> String) {}
  func error(_ s: @autoclosure () -> String) { FileHandle.standardError.write(Data(("error: " + s() + "\n").utf8)) }
  func info(_ s: @autoclosure () -> String) {}
  func debug(_ s: @autoclosure () -> String) {}
}

final class OSAllocatedUnfairLock<State: Sendable>: @unchecked Sendable {
  private var state: State
  private let lock = NSLock()
  init(initialState: State) { state = initialState }
  func withLock<R>(_ body: (inout State) throws -> R) rethrows -> R {
    lock.lock(); defer { lock.unlock() }
    return try body(&state)
  }
}
