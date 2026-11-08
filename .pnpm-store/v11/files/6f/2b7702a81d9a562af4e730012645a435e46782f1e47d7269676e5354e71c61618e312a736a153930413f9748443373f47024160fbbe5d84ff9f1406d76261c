import { getPatch, isValid } from 'verkit';
import { filterRevertedCommitsSync } from 'conventional-commits-filter';
import { stringify } from './utils.js';
export function getCommitGroups(commits, options) {
    const { groupBy, commitGroupsSort, commitsSort } = options;
    const commitGroups = [];
    const commitGroupsObj = commits.reduce((groups, commit) => {
        const key = commit[groupBy] || '';
        if (groups[key]) {
            groups[key].push(commit);
        }
        else {
            groups[key] = [commit];
        }
        return groups;
        // oxlint-disable-next-line typescript/no-unsafe-argument
    }, Object.create(null));
    Object.entries(commitGroupsObj).forEach(([title, commits]) => {
        if (commitsSort) {
            commits.sort(commitsSort);
        }
        commitGroups.push({
            title,
            commits
        });
    });
    if (commitGroupsSort) {
        commitGroups.sort(commitGroupsSort);
    }
    return commitGroups;
}
export function getNoteGroups(notes, options) {
    const { noteGroupsSort, notesSort } = options;
    const retGroups = [];
    notes.forEach((note) => {
        const { title } = note;
        let titleExists = false;
        retGroups.forEach((group) => {
            if (group.title === title) {
                titleExists = true;
                group.notes.push(note);
            }
        });
        if (!titleExists) {
            retGroups.push({
                title,
                notes: [note]
            });
        }
    });
    if (noteGroupsSort) {
        retGroups.sort(noteGroupsSort);
    }
    if (notesSort) {
        retGroups.forEach((group) => {
            group.notes.sort(notesSort);
        });
    }
    return retGroups;
}
export function getExtraContext(commits, notes, options) {
    return {
        // group `commits` by `options.groupBy`
        commitGroups: getCommitGroups(commits, options),
        // group `notes` for footer
        noteGroups: getNoteGroups(notes, options)
    };
}
/**
 * Get final context with default values.
 * @param context
 * @param options
 * @returns Final context with default values.
 */
export function getFinalContext(context, options) {
    const finalContext = {
        commit: 'commits',
        issue: 'issues',
        compare: 'compare',
        date: options.formatDate(new Date()),
        headerPartial: options.headerPartial,
        preamblePartial: options.preamblePartial,
        commitPartial: options.commitPartial,
        footerPartial: options.footerPartial,
        ...context
    };
    if (typeof finalContext.linkReferences !== 'boolean'
        && (finalContext.repository || finalContext.repoUrl)
        && finalContext.commit
        && finalContext.issue) {
        finalContext.linkReferences = true;
    }
    return finalContext;
}
/**
 * Get context prepared for template.
 * @param keyCommit
 * @param commits
 * @param filteredCommits
 * @param notes
 * @param context
 * @param options
 * @returns TemplateContext prepared for template.
 */
export async function getTemplateContext(keyCommit, commits, context, options) {
    const notes = [];
    const filteredCommits = (options.ignoreReverted
        ? Array.from(filterRevertedCommitsSync(commits))
        : commits).map(commit => ({
        ...commit,
        notes: commit.notes.map((note) => {
            const commitNote = {
                ...note,
                commit
            };
            notes.push(commitNote);
            return commitNote;
        })
    }));
    let templateContext = {
        ...context,
        ...keyCommit,
        ...getExtraContext(filteredCommits, notes, options)
    };
    if (keyCommit?.committerDate) {
        templateContext.date = keyCommit.committerDate;
    }
    if (typeof templateContext.version === 'string' && isValid(templateContext.version)) {
        templateContext.isPatch ||= getPatch(templateContext.version) !== 0;
    }
    templateContext = await options.finalizeContext(templateContext, options, filteredCommits, keyCommit, commits);
    options.debug(`Your final context is:\n${stringify(templateContext)}`);
    return templateContext;
}
//# sourceMappingURL=data:application/json;base64,eyJ2ZXJzaW9uIjozLCJmaWxlIjoiY29udGV4dC5qcyIsInNvdXJjZVJvb3QiOiIiLCJzb3VyY2VzIjpbIi4uL3NyYy9jb250ZXh0LnRzIl0sIm5hbWVzIjpbXSwibWFwcGluZ3MiOiJBQVNBLE9BQU8sRUFDTCxRQUFRLEVBQ1IsT0FBTyxFQUNSLE1BQU0sUUFBUSxDQUFBO0FBQ2YsT0FBTyxFQUFFLHlCQUF5QixFQUFFLE1BQU0sNkJBQTZCLENBQUE7QUFFdkUsT0FBTyxFQUFFLFNBQVMsRUFBRSxNQUFNLFlBQVksQ0FBQTtBQUV0QyxNQUFNLFVBQVUsZUFBZSxDQUM3QixPQUFpQixFQUNqQixPQUFtRjtJQUVuRixNQUFNLEVBQ0osT0FBTyxFQUNQLGdCQUFnQixFQUNoQixXQUFXLEVBQ1osR0FBRyxPQUFPLENBQUE7SUFDWCxNQUFNLFlBQVksR0FBMEIsRUFBRSxDQUFBO0lBQzlDLE1BQU0sZUFBZSxHQUFHLE9BQU8sQ0FBQyxNQUFNLENBQTJCLENBQUMsTUFBTSxFQUFFLE1BQU0sRUFBRSxFQUFFO1FBQ2xGLE1BQU0sR0FBRyxHQUFHLE1BQU0sQ0FBQyxPQUFPLENBQVcsSUFBSSxFQUFFLENBQUE7UUFFM0MsSUFBSSxNQUFNLENBQUMsR0FBRyxDQUFDLEVBQUUsQ0FBQztZQUNoQixNQUFNLENBQUMsR0FBRyxDQUFDLENBQUMsSUFBSSxDQUFDLE1BQU0sQ0FBQyxDQUFBO1FBQzFCLENBQUM7YUFBTSxDQUFDO1lBQ04sTUFBTSxDQUFDLEdBQUcsQ0FBQyxHQUFHLENBQUMsTUFBTSxDQUFDLENBQUE7UUFDeEIsQ0FBQztRQUVELE9BQU8sTUFBTSxDQUFBO1FBQ2YseURBQXlEO0lBQ3pELENBQUMsRUFBRSxNQUFNLENBQUMsTUFBTSxDQUFDLElBQUksQ0FBQyxDQUFDLENBQUE7SUFFdkIsTUFBTSxDQUFDLE9BQU8sQ0FBQyxlQUFlLENBQUMsQ0FBQyxPQUFPLENBQUMsQ0FBQyxDQUFDLEtBQUssRUFBRSxPQUFPLENBQUMsRUFBRSxFQUFFO1FBQzNELElBQUksV0FBVyxFQUFFLENBQUM7WUFDaEIsT0FB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