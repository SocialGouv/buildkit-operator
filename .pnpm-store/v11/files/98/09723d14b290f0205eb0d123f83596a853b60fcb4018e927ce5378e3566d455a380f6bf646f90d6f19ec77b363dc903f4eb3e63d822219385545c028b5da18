const nomatchRegex = /(?!.*)/;
function escape(string) {
    return string.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
function joinOr(parts) {
    return parts
        .map(val => (typeof val === 'string' ? escape(val.trim()) : val.source))
        .filter(Boolean)
        .join('|');
}
function getNotesRegex(noteKeywords, notesPattern) {
    if (!noteKeywords) {
        return nomatchRegex;
    }
    const noteKeywordsSelection = joinOr(noteKeywords);
    if (!notesPattern) {
        return new RegExp(`^(?:\\*\\s+)?(${noteKeywordsSelection}):\\s*(.*)`, 'i');
    }
    return notesPattern(noteKeywordsSelection);
}
function getReferencePartsRegex(issuePrefixes, issuePrefixesCaseSensitive) {
    if (!issuePrefixes) {
        return nomatchRegex;
    }
    // Sticky, so a search starts where the last reference ended and `.*?` takes the text
    // up to the next one: a line is scanned once, not again from every position.
    const flags = issuePrefixesCaseSensitive ? 'gy' : 'giy';
    const repositoryChar = '[\\w-\\.\\/]';
    // A reference starts a word, and its repository is the whole word before the prefix,
    // so `gh-` is not a prefix inside of `tough-cookie` or `high-5`.
    // An issue has a digit, so `#general` is not a reference.
    return new RegExp(`.*?(?<!${repositoryChar})(?:(${repositoryChar}+)(?!${repositoryChar}))?(${joinOr(issuePrefixes)})((?=[\\w-]*\\d)[\\w-]+)(?=\\s|$|[,;.)\\]])`, flags);
}
function getReferencesRegex(referenceActions, separator = '') {
    if (!referenceActions) {
        // matches everything
        return /()(.+)/gi;
    }
    const joinedKeywords = joinOr(referenceActions);
    // Keywords are whole words: `prefix #1` has no `fix` action,
    // and `fixups` does not end the references of the previous keyword.
    const nextKeyword = `(?<!\\w)(?:${joinedKeywords})(?!\\w)`;
    return new RegExp(`(?<!\\w)(${joinedKeywords})${separator}(?:\\s+(.*?))(?=${nextKeyword}|$)`, 'gi');
}
function getFooterTokenRegex(issuePrefixes) {
    const issuePrefixSeparator = issuePrefixes
        ? `|\\s+(?:${joinOr(issuePrefixes)})`
        : '';
    // Footers follow the git trailer convention: the token starts at the
    // beginning of the line, indented `key: value` lines are not footers.
    return new RegExp(`^(?:BREAKING CHANGE|[\\w-]+)(?::\\s+${issuePrefixSeparator}).+`, 'i');
}
/**
 * Make the regexes used to parse a commit.
 * @param options
 * @returns Regexes.
 */
export function getParserRegexes(options = {}) {
    const notes = getNotesRegex(options.noteKeywords, options.notesPattern);
    const referenceParts = getReferencePartsRegex(options.issuePrefixes, options.issuePrefixesCaseSensitive);
    const references = getReferencesRegex(options.referenceActions);
    // In a footer the keyword is a token, which may be followed by a colon: `Closes: #1`
    const footerReferences = getReferencesRegex(options.referenceActions, ':?');
    const footerToken = getFooterTokenRegex(options.issuePrefixes);
    return {
        notes,
        referenceParts,
        references,
        footerReferences,
        footerToken,
        mentions: /@([\w-]+)/g,
        url: /\b(?:https?):\/\/(?:www\.)?([-a-zA-Z0-9@:%_+.~#?&//=])+\b/
    };
}
//# sourceMappingURL=data:application/json;base64,eyJ2ZXJzaW9uIjozLCJmaWxlIjoicmVnZXguanMiLCJzb3VyY2VSb290IjoiIiwic291cmNlcyI6WyIuLi9zcmMvcmVnZXgudHMiXSwibmFtZXMiOltdLCJtYXBwaW5ncyI6IkFBS0EsTUFBTSxZQUFZLEdBQUcsUUFBUSxDQUFBO0FBRTdCLFNBQVMsTUFBTSxDQUFDLE1BQWM7SUFDNUIsT0FBTyxNQUFNLENBQUMsT0FBTyxDQUFDLHFCQUFxQixFQUFFLE1BQU0sQ0FBQyxDQUFBO0FBQ3RELENBQUM7QUFFRCxTQUFTLE1BQU0sQ0FBQyxLQUEwQjtJQUN4QyxPQUFPLEtBQUs7U0FDVCxHQUFHLENBQUMsR0FBRyxDQUFDLEVBQUUsQ0FBQyxDQUFDLE9BQU8sR0FBRyxLQUFLLFFBQVEsQ0FBQyxDQUFDLENBQUMsTUFBTSxDQUFDLEdBQUcsQ0FBQyxJQUFJLEVBQUUsQ0FBQyxDQUFDLENBQUMsQ0FBQyxHQUFHLENBQUMsTUFBTSxDQUFDLENBQUM7U0FDdkUsTUFBTSxDQUFDLE9BQU8sQ0FBQztTQUNmLElBQUksQ0FBQyxHQUFHLENBQUMsQ0FBQTtBQUNkLENBQUM7QUFFRCxTQUFTLGFBQWEsQ0FDcEIsWUFBNkMsRUFDN0MsWUFBb0Q7SUFFcEQsSUFBSSxDQUFDLFlBQVksRUFBRSxDQUFDO1FBQ2xCLE9BQU8sWUFBWSxDQUFBO0lBQ3JCLENBQUM7SUFFRCxNQUFNLHFCQUFxQixHQUFHLE1BQU0sQ0FBQyxZQUFZLENBQUMsQ0FBQTtJQUVsRCxJQUFJLENBQUMsWUFBWSxFQUFFLENBQUM7UUFDbEIsT0FBTyxJQUFJLE1BQU0sQ0FBQyxpQkFBaUIscUJBQXFCLFlBQVksRUFBRSxHQUFHLENBQUMsQ0FBQTtJQUM1RSxDQUFDO0lBRUQsT0FBTyxZQUFZLENBQUMscUJBQXFCLENBQUMsQ0FBQTtBQUM1QyxDQUFDO0FBRUQsU0FBUyxzQkFBc0IsQ0FDN0IsYUFBOEMsRUFDOUMsMEJBQStDO0lBRS9DLElBQUksQ0FBQyxhQUFhLEVBQUUsQ0FBQztRQUNuQixPQUFPLFlBQVksQ0FBQTtJQUNyQixDQUFDO0lBRUQscUZBQXFGO0lBQ3JGLDZFQUE2RTtJQUM3RSxNQUFNLEtBQUssR0FBRywwQkFBMEIsQ0FBQyxDQUFDLENBQUMsSUFBSSxDQUFDLENBQUMsQ0FBQyxLQUFLLENBQUE7SUFDdkQsTUFBTSxjQUFjLEdBQUcsY0FBYyxDQUFBO0lBRXJDLHFGQUFxRjtJQUNyRixpRUFBaUU7SUFDakUsMERBQTBEO0lBQzFELE9BQU8sSUFBSSxNQUFNLENBQUMsVUFBVSxjQUFjLFFBQVEsY0FBYyxRQUFRLGNBQWMsT0FBTyxNQUFNLENBQUMsYUFBYSxDQUFDLDZDQUE2QyxFQUFFLEtBQUssQ0FBQyxDQUFBO0FBQ3pLLENBQUM7QUFFRCxTQUFTLGtCQUFrQixDQUN6QixnQkFBaUQsRUFDakQsU0FBUyxHQUFHLEVBQUU7SUFFZCxJQUFJLENBQUMsZ0JBQWdCLEVBQUUsQ0FBQztRQUN0QixxQkFBcUI7UUFDckIsT0FBTyxVQUFVLENBQUE7SUFDbkIsQ0FBQztJQUVELE1BQU0sY0FBYyxHQUFHLE1BQU0sQ0FBQyxnQkFBZ0IsQ0FBQyxDQUFBO0lBQy9DLDZEQUE2RDtJQUM3RCxvRUFBb0U7SUFDcEUsTUFBTSxXQUFXLEdBQUcsY0FBYyxjQUFjLFVBQVUsQ0FBQTtJQUUxRCxPQUFPLElBQUksTUFBTSxDQUFDLFlBQVksY0FBYyxJQUFJLFNBQVMsbUJBQW1CLFdBQVcsS0FBSyxFQUFFLElBQUksQ0FBQyxDQUFBO0FBQ3JHLENBQUM7QUFFRCxTQUFTLG1CQUFtQixDQUMxQixhQUE4QztJQUU5QyxNQUFNLG9CQUFvQixHQUFHLGFBQWE7UUFDeEMsQ0FBQyxDQUFDLFdBQVcsTUFBTSxDQUFDLGFBQWEsQ0FBQyxHQUFHO1FBQ3JDLENBQUMsQ0FBQyxFQUFFLENBQUE7SUFFTixxRUFBcUU7SUFDckUsc0VBQXNFO0lBQ3RFLE9BQU8sSUFBSSxNQUFNLENBQUMsdUNBQXVDLG9CQUFvQixLQUFLLEVBQUUsR0FBRyxDQUFDLENBQUE7QUFDMUYsQ0FBQztBQUVEOzs7O0dBSUc7QUFDSCxNQUFNLFVBQVUsZ0JBQWdCLENBQzlCLE9BQU8sR0FBK0gsRUFBRTtJQUV4SSxNQUFNLEtBQUssR0FBRyxhQUFhLENBQUMsT0FBTyxDQUFDLFlBQVksRUFBRSxPQUFPLENBQUMsWUFBWSxDQUFDLENBQUE7SUFDdkUsTUFBTSxjQUFjLEdBQUcsc0JBQXNCLENBQUMsT0FBTyxDQUFDLGFBQWEsRUFBRSxPQUFPLENBQUMsMEJBQTBCLENBQUMsQ0FBQTtJQUN4RyxNQUFNLFVBQVUsR0FBRyxrQkFBa0IsQ0FBQyxPQUFPLENBQUMsZ0JBQWdCLENBQUMsQ0FBQTtJQUMvRCxxRkFBcUY7SUFDckYsTUFBTSxnQkFBZ0IsR0FBRyxrQkFBa0IsQ0FBQyxPQUFPLENBQUMsZ0JBQWdCLEVBQUUsSUFBSSxDQUFDLENBQUE7SUFDM0UsTUFBTSxXQUFXLEdBQUcsbUJBQW1CLENBQUMsT0FBTyxDQUFDLGFBQWEsQ0FBQyxDQUFBO0lBRTlELE9BQU87UUFDTCxLQUFLO1FBQ0wsY0FBYztRQUNkLFVBQVU7UUFDVixnQkFBZ0I7UUFDaEIsV0FBVztRQUNYLFFBQVEsRUFBRSxZQUFZO1FBQ3RCLEdBQUcsRUFBRSwyREFBMkQ7S0FDakUsQ0FBQTtBQUNILENBQUMifQ==