import Foundation

// Linux Foundation has no Markdown parser for AttributedString; the app
// only uses it for display, so keep the text as is.
extension AttributedString {
  struct MarkdownParsingOptions {
    enum InterpretedSyntax { case full, inlineOnly, inlineOnlyPreservingWhitespace }
    init(allowsExtendedAttributes: Bool = false, interpretedSyntax: InterpretedSyntax = .full) {}
  }
  init(markdown: String, options: MarkdownParsingOptions) throws { self.init(markdown) }
}

public typealias LocalizedStringKey = String

// Display-only formatter that Foundation on Linux lacks.
final class RelativeDateTimeFormatter: @unchecked Sendable {
  enum UnitsStyle { case full, short, abbreviated, spellOut }
  enum DateTimeStyle { case numeric, named }
  enum Context { case listItem, standalone, beginningOfSentence, middleOfSentence, dynamic, unknown }
  var unitsStyle: UnitsStyle = .full
  var dateTimeStyle: DateTimeStyle = .numeric
  var formattingContext: Context = .unknown
  func localizedString(for date: Date, relativeTo ref: Date) -> String {
    "\(Int(ref.timeIntervalSince(date)))s ago"
  }
}
