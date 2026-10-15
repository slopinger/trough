import Foundation

// Apple-only bits the entities touch for display.
public struct UIImage {
  public enum RenderingMode { case alwaysTemplate, alwaysOriginal, automatic }
  public init?(systemName: String) {}
  public func withRenderingMode(_ m: RenderingMode) -> UIImage { self }
}
public enum L10n {
  public enum Scene { public enum Compose { public enum Visibility {
    public static let `public` = "Public", unlisted = "Unlisted", `private` = "Followers", direct = "Direct"
  } } }
}

extension UserDefaults {
  // MastodonCommon's app-group defaults
  public static let shared = UserDefaults.standard
  public var preferredStaticAvatar: Bool { false }
}
